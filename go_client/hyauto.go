package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// hyauto.go — автоподбор полосы для Brutal.
//
// Зачем. Brutal шлёт с заданной скоростью и не считает потери сигналом
// перегрузки — на душимом канале это ровнее BBR. Но он требует числа, а у
// мобильного пользователя такого числа нет: скорость меняется от соты к соте.
// Поэтому полосу подбираем на ходу: меряем, ставим чуть ниже измеренного и
// пересматриваем.
//
// Как. Обычный AIMD с защитой по задержке — скромный рост, резкий сброс:
//
//   потери выше порога ИЛИ RTT раздулся против минимального → режем на 30%
//   иначе, если реально выбираем больше utilizationRaise от установки → +25%
//   иначе держим: упёрлись не в канал, а в отсутствие трафика
//
// Сигналы:
//   - фактическая скорость — счётчики на сокете QUIC (см. countingPacketConn);
//   - RTT и потери — самостоятельные пробы адаптивной маскировки
//     (aiobfs.Shaper.PathStats, сводка в pathstats.go).
//
// Без адаптивной маскировки замеров RTT/потерь нет, и остаётся только рост по
// утилизации. Чтобы в этом случае контроллер не разгонялся в пустоту, у него
// есть потолок и требование, чтобы рост подтверждался ростом скорости:
// поднятая планка, не давшая прибавки, откатывается.
//
// Смена полосы = новая сессия Hysteria2: bps у Brutal задаётся при
// рукопожатии и на живом соединении не меняется (BrutalSender.bps приватное,
// сеттера нет). Поэтому меняем редко и только на заметную величину — см.
// minChangeRatio и minChangeInterval, а пересоздание идёт с передачей
// эстафеты (hysteria.go), чтобы открытые соединения не рвались.

const (
	autoEvalInterval = 10 * time.Second
	autoMinMbps      = 2.0
	autoMaxMbps      = 300.0
	// Порог потерь намеренно высокий. Пробы адаптивной маскировки — мелкие
	// пакеты, релей режет их охотнее полезных, а ответ, пришедший на
	// границе окна, засчитывается как потеря. Ставить сюда «канонические»
	// 2-3% значит резать полосу по шуму измерения: в логе так и вышло —
	// 12.5% потерь по пробам при живом туннеле и 50 МБ переданных данных.
	autoLossThreshold = 0.20
	// И только если проб в окне достаточно, чтобы доля вообще что-то
	// значила: одна сессия даёт четыре пробы (шаг 25%), девять — около 36.
	autoMinProbes    = 12
	autoRTTInflation = 1.8  // во столько раз RTT может превысить минимум
	autoRTTSlackMs   = 60.0 // плюс запас на дрожание мобильной сети
	utilizationRaise = 0.80 // выбираем больше 80% установки — просим ещё
	// Ниже этой утилизации сигналы перегрузки не наши: если мы почти не
	// шлём, а потери есть — это состояние канала, и резать наш потолок
	// бессмысленно (а при переподключении воркеров ещё и вредно).
	utilizationFloor = 0.25
	raiseFactor      = 1.25
	recoveryFactor   = 1.10 // медленный возврат вверх на здоровом пути
	// Во сколько раз установка может превышать лучшую достигнутую скорость.
	// Brutal и так шлёт «сколько сказано»; задирать потолок в разы выше
	// того, что канал когда-либо давал, — значит просто отключить контроль.
	headroomFactor = 2.0
	bestDecay      = 0.98
	cutFactor      = 0.70
	minChangeRatio = 0.25 // менять сессию только при изменении ≥25%
	// Под нагрузкой планка выше: пересоздание рвёт открытые соединения
	// (одновременно двух сессий туннель не выдерживает), и ради небольшой
	// прибавки ломать скачивание не стоит.
	minChangeRatioBusy = 0.50
	minChangeInterval  = 45 * time.Second
	// Стартовые значения намеренно скромные: лучше несколько секунд
	// недобора, чем сразу забитый канал и переподключение.
	autoStartUpMbps   = 5.0
	autoStartDownMbps = 15.0
)

// bandwidthController держит текущую установку полосы и пересматривает её по
// замерам. Все методы вызываются из одной горутины супервизора, внешней
// синхронизации не требуют.
type bandwidthController struct {
	upMbps   float64
	downMbps float64

	// appliedUp/appliedDown — то, с чем реально поднята текущая сессия.
	appliedUp   float64
	appliedDown float64

	rttMinMs     float64
	lastChangeAt time.Time
	// lossEWMA сглаживает долю потерь по окнам: одно окно — это единицы
	// проб, по нему решать нельзя.
	lossEWMA     float64
	haveLossEWMA bool
	// Лучшая наблюдавшаяся скорость с медленным затуханием — потолок для
	// роста. Без него «возвращаю полосу» разгонял установку до 111 Мбит/с
	// при реальных 12: полоса переставала что-либо значить, а каждое
	// изменение стоит пересоздания сессии.
	bestUpBps   float64
	bestDownBps float64
	// busy: идёт заметный обмен, значит пересоздание сессии кого-то оборвёт.
	busy bool
	// Скорость и установка на момент последнего повышения — чтобы понять,
	// не сделало ли оно хуже, когда судить по RTT/потерям нечем.
	lastRaiseDownBps float64
	preRaiseDownMbps float64
	raisePending     bool
}

func newBandwidthController() *bandwidthController {
	return &bandwidthController{
		upMbps:      autoStartUpMbps,
		downMbps:    autoStartDownMbps,
		appliedUp:   autoStartUpMbps,
		appliedDown: autoStartDownMbps,
		rttMinMs:    math.Inf(1),
	}
}

// pathObservation — то, что контроллер знает о пути за последнее окно.
type pathObservation struct {
	upBps          float64 // фактическая скорость наружу, бит/с
	downBps        float64 // фактическая скорость внутрь, бит/с
	rttMs          float64
	haveRTT        bool // RTT реально измерялся, а не подставлен «худший случай»
	loss           float64
	probes         int // сколько проб в окне: по нулю судить о потерях нельзя
	networkChanged bool
}

// decision — что контроллер решил на этом шаге.
type decision struct {
	upMbps   float64
	downMbps float64
	apply    bool   // нужна ли новая сессия
	reason   string // человеческое объяснение для лога
}

// step пересматривает полосу по одному окну замеров.
func (c *bandwidthController) step(now time.Time, obs pathObservation) decision {
	// Смена сети (другая сота, переход Wi-Fi ↔ LTE) обнуляет накопленное:
	// прежний минимум RTT и прежняя оценка канала к новой сети отношения не
	// имеют, а держаться за них — значит долго ехать на чужих цифрах.
	if obs.networkChanged {
		c.rttMinMs = math.Inf(1)
		c.upMbps = autoStartUpMbps
		c.downMbps = autoStartDownMbps
		c.raisePending = false
		return c.commit(now, "сменилась сеть, начинаем подбор заново", true)
	}

	if obs.haveRTT && obs.rttMs > 0 && obs.rttMs < c.rttMinMs {
		c.rttMinMs = obs.rttMs
	}

	// Затухание не даёт разовому всплеску закрепить потолок навсегда:
	// за десяток окон оценка сползает к текущей реальности.
	c.bestUpBps = math.Max(obs.upBps, c.bestUpBps*bestDecay)
	c.bestDownBps = math.Max(obs.downBps, c.bestDownBps*bestDecay)

	if obs.probes >= autoMinProbes {
		if c.haveLossEWMA {
			c.lossEWMA = 0.6*c.lossEWMA + 0.4*obs.loss
		} else {
			c.lossEWMA = obs.loss
			c.haveLossEWMA = true
		}
	}

	upUtil := obs.upBps / (c.upMbps * 1e6)
	downUtil := obs.downBps / (c.downMbps * 1e6)
	util := math.Max(upUtil, downUtil)

	c.busy = util >= utilizationFloor

	// Пока мы толком не шлём, сигналы перегрузки относятся не к нам:
	// потери и раздутый RTT в этот момент говорят о состоянии канала или о
	// переподключении воркеров, а урезание нашего потолка ничего не лечит.
	driving := util >= utilizationFloor

	congested := false
	reason := ""
	switch {
	case driving && c.haveLossEWMA && c.lossEWMA > autoLossThreshold:
		congested = true
		reason = fmt.Sprintf("потери %.0f%%", c.lossEWMA*100)
	case driving && obs.haveRTT && !math.IsInf(c.rttMinMs, 1) &&
		obs.rttMs > c.rttMinMs*autoRTTInflation+autoRTTSlackMs:
		congested = true
		reason = fmt.Sprintf("RTT %.0f мс против %.0f мс в лучшем случае", obs.rttMs, c.rttMinMs)
	case c.raisePending && obs.downBps > 0 && obs.downBps < c.lastRaiseDownBps*0.9:
		// Подняли планку — и скорость упала. Вот это перебор: откатываемся
		// ровно к прежнему значению, а не режем ещё на 30%.
		//
		// Раньше здесь резали всё, что «не дало прибавки». Это было неверно:
		// если канал ровно держит свои 80 Мбит/с, отсутствие прибавки значит
		// лишь, что упёрлись не в нашу планку, — а резали мы при этом ниже
		// заведомо рабочего уровня, по кругу, вплоть до пола.
		c.downMbps = clampMbps(c.preRaiseDownMbps)
		c.raisePending = false
		return c.commit(now, "повышение ухудшило скорость, возвращаю прежнюю полосу", false)
	}

	if congested {
		c.upMbps = clampMbps(c.upMbps * cutFactor)
		c.downMbps = clampMbps(c.downMbps * cutFactor)
		c.raisePending = false
		return c.commit(now, "снижаю: "+reason, false)
	}

	if upUtil >= utilizationRaise || downUtil >= utilizationRaise {
		if upUtil >= utilizationRaise {
			c.upMbps = c.limitByObserved(c.upMbps*raiseFactor, c.bestUpBps, autoStartUpMbps)
		}
		if downUtil >= utilizationRaise {
			raised := c.limitByObserved(c.downMbps*raiseFactor, c.bestDownBps, autoStartDownMbps)
			if raised > c.downMbps {
				c.preRaiseDownMbps = c.downMbps
				c.lastRaiseDownBps = obs.downBps
				c.raisePending = true
			}
			c.downMbps = raised
		}
		return c.commit(now, "упёрлись в установленную полосу, поднимаю", false)
	}

	c.raisePending = false

	// Ни перегрузки, ни упора в полосу: трафика просто мало. Полосу при
	// этом медленно возвращаем вверх, если путь выглядит здоровым — иначе
	// один неудачный отрезок оставил бы нас на заниженном потолке до конца
	// сессии. Само по себе поднятие потолка ничего не шлёт, а применяется
	// оно всё равно по общим правилам (заметное изменение + выдержка).
	healthy := (!c.haveLossEWMA || c.lossEWMA < autoLossThreshold/2) &&
		(!obs.haveRTT || math.IsInf(c.rttMinMs, 1) ||
			obs.rttMs < c.rttMinMs*autoRTTInflation)
	if healthy {
		up := c.limitByObserved(c.upMbps*recoveryFactor, c.bestUpBps, autoStartUpMbps)
		down := c.limitByObserved(c.downMbps*recoveryFactor, c.bestDownBps, autoStartDownMbps)
		if up > c.upMbps || down > c.downMbps {
			c.upMbps, c.downMbps = up, down
			return c.commit(now, "путь спокойный, возвращаю полосу", false)
		}
	}
	return decision{upMbps: c.upMbps, downMbps: c.downMbps}
}

// limitByObserved не даёт установке уходить дальше headroomFactor от лучшей
// достигнутой скорости: запас нужен, чтобы было куда расти, а отрыв в разы
// превращает Brutal в «шли сколько хочешь».
func (c *bandwidthController) limitByObserved(target, bestBps, floorMbps float64) float64 {
	ceiling := math.Max(bestBps/1e6*headroomFactor, floorMbps)
	return clampMbps(math.Min(target, ceiling))
}

// commit решает, стоит ли ради нового значения пересоздавать сессию.
func (c *bandwidthController) commit(now time.Time, reason string, force bool) decision {
	d := decision{upMbps: c.upMbps, downMbps: c.downMbps, reason: reason}
	ratio := minChangeRatio
	if c.busy {
		ratio = minChangeRatioBusy
	}
	changed := relDiff(c.upMbps, c.appliedUp) >= ratio ||
		relDiff(c.downMbps, c.appliedDown) >= ratio
	tooSoon := !force && !c.lastChangeAt.IsZero() && now.Sub(c.lastChangeAt) < minChangeInterval
	if (changed || force) && !tooSoon {
		c.appliedUp = c.upMbps
		c.appliedDown = c.downMbps
		c.lastChangeAt = now
		d.apply = true
	}
	return d
}

func relDiff(a, b float64) float64 {
	if b <= 0 {
		return 1
	}
	return math.Abs(a-b) / b
}

func clampMbps(v float64) float64 {
	if v < autoMinMbps {
		return autoMinMbps
	}
	if v > autoMaxMbps {
		return autoMaxMbps
	}
	return v
}

// ── Супервизор сессии Hysteria2 ──────────────────────────────────────────────

const (
	// Одновременно живой может быть ровно одна сессия Hysteria2.
	//
	// Диспетчер туннеля (dispatcher.go) держит ОДИН адрес локального
	// клиента: clientAddr перезаписывается адресом последнего отправителя,
	// и все ответы из туннеля уходят только ему. Две сессии — это два
	// локальных UDP-сокета, которые дерутся за этот слот, и обе теряют
	// ответы: в бою это дало шторм "handshake did not complete in time".
	// Поэтому старую сессию закрываем ДО создания новой, а плату за это
	// (разрыв открытых соединений) снижаем тем, что меняем полосу редко.
	closeOldBeforeNew = true
	// Пауза перед повтором, если сессия не поднялась.
	retryDelay = 3 * time.Second
	// Если замеров пути не было столько времени, а потом они снова пошли —
	// считаем, что телефон переехал в другую сеть.
	pathGapAsNetworkChange = 25 * time.Second
)

// runHysteriaSupervisor держит живую сессию Hysteria2 и, в режиме автоподбора,
// пересоздаёт её под новую полосу. Возвращается по отмене ctx.
func runHysteriaSupervisor(ctx context.Context, p HysteriaParams, holder *hyClientHolder, auto bool) {
	ctrl := newBandwidthController()
	if auto {
		p.UpMbps = uint64(math.Round(ctrl.upMbps))
		p.DownMbps = uint64(math.Round(ctrl.downMbps))
		log.Printf("[HY2] Автоподбор полосы включён, старт с ↑%.0f / ↓%.0f Мбит/с", ctrl.upMbps, ctrl.downMbps)
	}

	for ctx.Err() == nil {
		// Сначала освобождаем локальный сокет: пока жива старая сессия,
		// новая не сможет получать ответы из туннеля (см. closeOldBeforeNew).
		if old := holder.publish(nil); old != nil {
			_ = old.Close()
		}

		c, err := newHysteriaClient(p)
		if err != nil {
			log.Printf("[HY2] %v — повтор через %v", err, retryDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			continue
		}

		if previous := holder.publish(c); previous != nil {
			_ = previous.Close()
		}

		superviseSession(ctx, holder, ctrl, auto, &p)
		if ctx.Err() != nil {
			_ = c.Close()
			return
		}
	}
}

// superviseSession следит за одной сессией: печатает скорость и, в режиме
// автоподбора, решает, когда пора менять полосу. Возвращается, когда сессия
// умерла или нужна новая с другими параметрами (тогда p уже обновлён).
func superviseSession(
	ctx context.Context,
	holder *hyClientHolder,
	ctrl *bandwidthController,
	auto bool,
	p *HysteriaParams,
) {
	ticker := time.NewTicker(autoEvalInterval)
	defer ticker.Stop()

	dead := holder.deadCh()
	globalHyMeter.sample() // задаём точку отсчёта, первое окно считаем с неё

	var lastPathAt time.Time
	havePathEver := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-dead:
			log.Printf("[HY2] Сессия потеряна, поднимаю заново")
			return
		case <-ticker.C:
		}

		upBps, downBps := globalHyMeter.sample()
		logHySpeed(upBps, downBps)

		if !auto {
			continue
		}

		snap := globalPath.snapshot()
		now := time.Now()
		networkChanged := false
		if snap.sessions > 0 {
			if havePathEver && !lastPathAt.IsZero() && now.Sub(lastPathAt) > pathGapAsNetworkChange {
				// Замеры пропадали надолго и вернулись — почти наверняка
				// переподключение в другой сети.
				networkChanged = true
			}
			lastPathAt = now
			havePathEver = true
		}

		d := ctrl.step(now, pathObservation{
			upBps:          upBps,
			downBps:        downBps,
			rttMs:          snap.rttMs,
			haveRTT:        snap.haveRTT,
			loss:           snap.loss,
			probes:         snap.probes,
			networkChanged: networkChanged,
		})
		if !d.apply {
			continue
		}

		log.Printf("[HY2] Полоса: ↑%.0f / ↓%.0f Мбит/с (%s)", d.upMbps, d.downMbps, d.reason)
		p.UpMbps = uint64(math.Round(d.upMbps))
		p.DownMbps = uint64(math.Round(d.downMbps))
		return
	}
}

// logHySpeed печатает текущую скорость и пик за сессию.
//
// Пик нужен потому, что в приложении эта строка обновляется на месте: без
// него видно только последний замер, и после паузы в трафике лог показывает
// «0.1 Мбит/с», как будто больше и не было. Молчим на простое, чтобы не
// забивать лог нулями.
var hyPeakUpBps, hyPeakDownBps float64

func logHySpeed(upBps, downBps float64) {
	if upBps > hyPeakUpBps {
		hyPeakUpBps = upBps
	}
	if downBps > hyPeakDownBps {
		hyPeakDownBps = downBps
	}
	if upBps < 50_000 && downBps < 50_000 {
		return
	}
	log.Printf("[HY2] Скорость: ↓%.1f / ↑%.1f Мбит/с (пик ↓%.1f / ↑%.1f)",
		downBps/1e6, upBps/1e6, hyPeakDownBps/1e6, hyPeakUpBps/1e6)
}

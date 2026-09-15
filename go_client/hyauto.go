package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
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
	utilizationFloor  = 0.25
	raiseFactor       = 1.25
	recoveryFactor    = 1.10 // медленный возврат вверх на здоровом пути
	cutFactor         = 0.70
	minChangeRatio    = 0.25 // менять сессию только при изменении ≥25%
	minChangeInterval = 45 * time.Second
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
	// Скорость на момент последнего повышения — чтобы понять, дало ли оно
	// прибавку, когда судить по RTT/потерям нечем.
	lastRaiseDownBps float64
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
	case c.raisePending && obs.downBps > 0 && obs.downBps < c.lastRaiseDownBps*1.05:
		// Подняли планку, а скорость не выросла — значит упёрлись не в неё.
		congested = true
		reason = "повышение не дало прибавки"
	}

	if congested {
		c.upMbps = clampMbps(c.upMbps * cutFactor)
		c.downMbps = clampMbps(c.downMbps * cutFactor)
		c.raisePending = false
		return c.commit(now, "снижаю: "+reason, false)
	}

	if upUtil >= utilizationRaise || downUtil >= utilizationRaise {
		if upUtil >= utilizationRaise {
			c.upMbps = clampMbps(c.upMbps * raiseFactor)
		}
		if downUtil >= utilizationRaise {
			c.downMbps = clampMbps(c.downMbps * raiseFactor)
			c.lastRaiseDownBps = obs.downBps
			c.raisePending = true
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
	if healthy && (c.upMbps < autoMaxMbps || c.downMbps < autoMaxMbps) {
		c.upMbps = clampMbps(c.upMbps * recoveryFactor)
		c.downMbps = clampMbps(c.downMbps * recoveryFactor)
		return c.commit(now, "путь спокойный, возвращаю полосу", false)
	}
	return decision{upMbps: c.upMbps, downMbps: c.downMbps}
}

// commit решает, стоит ли ради нового значения пересоздавать сессию.
func (c *bandwidthController) commit(now time.Time, reason string, force bool) decision {
	d := decision{upMbps: c.upMbps, downMbps: c.downMbps, reason: reason}
	changed := relDiff(c.upMbps, c.appliedUp) >= minChangeRatio ||
		relDiff(c.downMbps, c.appliedDown) >= minChangeRatio
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
	// Сколько жить старой сессии после смены полосы: соединения, открытые в
	// ней, должны успеть договорить. Новые уже идут в новую сессию.
	handoverGrace = 90 * time.Second
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
			// Старую сессию закрываем не сразу: в ней ещё живут открытые
			// соединения (см. handoverGrace).
			go func(old hyclient.Client) {
				select {
				case <-ctx.Done():
				case <-time.After(handoverGrace):
				}
				_ = old.Close()
			}(previous)
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

package main

import (
	"math"
	"testing"
	"time"
)

func mbps(v float64) float64 { return v * 1e6 }

// Упёрлись в установленную полосу при здоровом пути — планка растёт.
func TestControllerRaisesWhenSaturated(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	startDown := c.downMbps

	d := c.step(now, pathObservation{
		downBps: mbps(startDown * 0.9),
		rttMs:   50, loss: 0, haveRTT: true,
	})
	if d.downMbps <= startDown {
		t.Fatalf("полоса не выросла: было %.1f, стало %.1f", startDown, d.downMbps)
	}
}

// Устойчивые большие потери при активной отдаче — режем.
func TestControllerCutsOnSustainedLoss(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 60, 60

	var d decision
	for i := 0; i < 4; i++ {
		d = c.step(now.Add(time.Duration(i)*autoEvalInterval), pathObservation{
			downBps: mbps(30), // половина полосы: шлём, но не упёрлись
			rttMs:   60, haveRTT: true,
			loss: 0.45, probes: 36,
		})
	}
	if d.downMbps >= 60 {
		t.Fatalf("полоса не снижена при устойчивых потерях 45%%: %.1f", d.downMbps)
	}
}

// Умеренные потери по пробам — не повод резать.
//
// Ровно это и случилось в бою: 12.5% потерь ПРОБ при живом туннеле и 50 МБ
// переданных данных уронили полосу до 4/10 Мбит/с. Пробы — мелкие пакеты,
// релей режет их охотнее полезных, а ответ на границе окна засчитывается как
// потеря.
func TestControllerIgnoresProbeNoise(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 60, 60

	for i := 0; i < 5; i++ {
		d := c.step(now.Add(time.Duration(i)*autoEvalInterval), pathObservation{
			downBps: mbps(30),
			rttMs:   60, haveRTT: true,
			loss: 0.125, probes: 36,
		})
		if d.downMbps < 60 {
			t.Fatalf("полоса снижена по шуму измерения (12.5%% потерь проб): %.1f", d.downMbps)
		}
	}
}

// Проб слишком мало — доля потерь ничего не значит, судить по ней нельзя.
func TestControllerIgnoresLossFromTooFewProbes(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 60, 60

	for i := 0; i < 5; i++ {
		d := c.step(now.Add(time.Duration(i)*autoEvalInterval), pathObservation{
			downBps: mbps(30),
			rttMs:   60, haveRTT: true,
			loss: 0.75, probes: 4, // одна сессия: шаг измерения 25%
		})
		if d.downMbps < 60 {
			t.Fatalf("полоса снижена по четырём пробам: %.1f", d.downMbps)
		}
	}
}

// Мы почти не шлём — значит перегрузка не наша, и резать наш потолок нечего.
// Так выглядит переподключение воркеров: потери есть, трафика нет.
func TestControllerIgnoresCongestionWhenNotDriving(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 60, 60

	for i := 0; i < 5; i++ {
		d := c.step(now.Add(time.Duration(i)*autoEvalInterval), pathObservation{
			downBps: mbps(2), // 3% от установленной полосы
			rttMs:   900, haveRTT: true,
			loss: 0.6, probes: 36,
		})
		if d.downMbps < 60 {
			t.Fatalf("полоса снижена, хотя мы почти ничего не слали: %.1f", d.downMbps)
		}
	}
}

// После вынужденного снижения полоса должна возвращаться, когда путь
// успокоился, — иначе один плохой отрезок оставит нас внизу навсегда.
func TestControllerRecoversOnQuietPath(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 6, 6
	c.upMbps, c.appliedUp = 3, 3

	for i := 0; i < 5; i++ {
		c.step(now.Add(time.Duration(i)*autoEvalInterval), pathObservation{
			downBps: mbps(0.1),
			rttMs:   50, haveRTT: true,
			loss: 0, probes: 36,
		})
	}
	if c.downMbps <= 6 {
		t.Fatalf("полоса не восстанавливается на спокойном пути: %.1f", c.downMbps)
	}
}

// Раздувшийся RTT при нулевых потерях — тоже перегрузка (bufferbloat).
func TestControllerCutsOnBufferbloat(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 60, 60

	// Сначала пара окон со здоровым RTT, чтобы запомнился минимум.
	c.step(now, pathObservation{downBps: mbps(1), rttMs: 40, haveRTT: true})
	if math.IsInf(c.rttMinMs, 1) {
		t.Fatalf("минимальный RTT не запомнился")
	}

	d := c.step(now.Add(time.Minute), pathObservation{
		downBps: mbps(55), rttMs: 400, loss: 0, haveRTT: true,
	})
	if d.downMbps >= 60 {
		t.Fatalf("полоса не снижена при RTT 400 мс против 40 мс: %.1f", d.downMbps)
	}
}

// На простое контроллер не режет полосу: подбирать канал по паузам нельзя.
func TestControllerDoesNotCutWhenIdle(t *testing.T) {
	c := newBandwidthController()
	before := c.downMbps

	d := c.step(time.Now(), pathObservation{
		downBps: mbps(0.2), rttMs: 45, haveRTT: true, probes: 36,
	})
	if d.downMbps < before {
		t.Fatalf("на простое полоса снижена: %.1f -> %.1f", before, d.downMbps)
	}
}

// Без замеров RTT (маскировка выключена) рост подтверждается прибавкой
// скорости: поднятая планка, не давшая прироста, откатывается.
func TestControllerRollsBackUselessRaise(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()

	// Упёрлись — поднимаем. Замеров RTT/потерь нет: маскировка выключена.
	c.step(now, pathObservation{downBps: mbps(14)})
	raised := c.downMbps
	if raised <= autoStartDownMbps {
		t.Fatalf("ожидался рост: %.1f", raised)
	}

	// Скорость осталась прежней — откат.
	d := c.step(now.Add(autoEvalInterval), pathObservation{downBps: mbps(14)})
	if d.downMbps >= raised {
		t.Fatalf("бесполезное повышение не откатилось: %.1f -> %.1f", raised, d.downMbps)
	}
}

// Смена сети сбрасывает накопленное и применяется немедленно, минуя
// выдержку между переключениями.
func TestControllerResetsOnNetworkChange(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 120, 120
	c.rttMinMs = 20
	c.lastChangeAt = now // только что меняли — обычное изменение было бы отложено

	d := c.step(now, pathObservation{networkChanged: true})
	if d.downMbps != autoStartDownMbps {
		t.Fatalf("после смены сети ожидался старт с %.1f, получено %.1f", autoStartDownMbps, d.downMbps)
	}
	if !d.apply {
		t.Fatalf("смена сети должна применяться немедленно")
	}
	if !math.IsInf(c.rttMinMs, 1) {
		t.Fatalf("минимальный RTT от прежней сети не сброшен: %.0f", c.rttMinMs)
	}
}

// Мелкие изменения не пересоздают сессию: каждое пересоздание — новое
// рукопожатие QUIC.
func TestControllerDoesNotChurnOnSmallChanges(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	c.downMbps, c.appliedDown = 100, 100
	c.upMbps, c.appliedUp = 20, 20

	// +25% к downMbps: это ровно порог, но выдержка ещё не вышла.
	c.lastChangeAt = now
	d := c.step(now.Add(5*time.Second), pathObservation{downBps: mbps(95), rttMs: 50, haveRTT: true})
	if d.apply {
		t.Fatalf("сессию нельзя пересоздавать чаще, чем раз в %v", minChangeInterval)
	}
}

// Планка не уходит за разумные пределы ни вверх, ни вниз.
func TestControllerClampsBounds(t *testing.T) {
	c := newBandwidthController()
	now := time.Now()
	for i := 0; i < 50; i++ {
		c.step(now.Add(time.Duration(i)*time.Minute), pathObservation{
			downBps: mbps(c.downMbps * 0.95), upBps: mbps(c.upMbps * 0.95),
			rttMs: 30, haveRTT: true,
		})
	}
	if c.downMbps > autoMaxMbps || c.upMbps > autoMaxMbps {
		t.Fatalf("превышен потолок: ↑%.1f ↓%.1f", c.upMbps, c.downMbps)
	}
	for i := 0; i < 50; i++ {
		c.step(now.Add(time.Duration(100+i)*time.Minute), pathObservation{
			downBps: mbps(1), rttMs: 500, loss: 0.5, haveRTT: true,
		})
	}
	if c.downMbps < autoMinMbps || c.upMbps < autoMinMbps {
		t.Fatalf("провалились под пол: ↑%.1f ↓%.1f", c.upMbps, c.downMbps)
	}
}

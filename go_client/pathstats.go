package main

import (
	"sync"
	"time"
)

// pathstats.go — общая сводка о состоянии пути до сервера.
//
// Замеры делает адаптивная маскировка: её шейпер шлёт собственные пробы и по
// ответам считает RTT и долю потерь (aiobfs.Shaper.PathStats). Шейпер живёт в
// каждой TURN-сессии отдельно, а полосу Hysteria2 подбирает один контроллер на
// весь клиент — поэтому сессии складывают свои замеры сюда, а контроллер
// читает уже сводку.
//
// Почему медиана, а не среднее: сессий девять, и одна залипшая (воркер,
// которому не повезло с релеем) не должна тянуть общую картину. Медиана
// переживает такой выброс, среднее — нет.

const pathStatsTTL = 20 * time.Second

type pathSample struct {
	rttMs   float64
	haveRTT bool
	sent    int
	ponged  int
	at      time.Time
}

type pathMonitor struct {
	mu      sync.Mutex
	samples map[int]pathSample
}

var globalPath = &pathMonitor{samples: make(map[int]pathSample)}

// report кладёт свежий замер от сессии sessionID.
func (m *pathMonitor) report(sessionID int, rttMs float64, haveRTT bool, sent, ponged int) {
	m.mu.Lock()
	m.samples[sessionID] = pathSample{
		rttMs:   rttMs,
		haveRTT: haveRTT,
		sent:    sent,
		ponged:  ponged,
		at:      time.Now(),
	}
	m.mu.Unlock()
}

// forget убирает замеры завершившейся сессии, чтобы они не участвовали в
// сводке после её смерти.
func (m *pathMonitor) forget(sessionID int) {
	m.mu.Lock()
	delete(m.samples, sessionID)
	m.mu.Unlock()
}

// pathSnapshot — сводка по живым сессиям.
type pathSnapshot struct {
	rttMs    float64 // медиана по сессиям, где RTT реально измерялся
	haveRTT  bool
	loss     float64 // суммарная доля потерь проб
	probes   int     // сколько проб суммарно ушло за окно
	sessions int
}

// snapshot складывает замеры всех живых сессий.
//
// RTT — медиана: одна залипшая сессия не должна тянуть картину. Потери —
// именно СУММА счётчиков, а не медиана долей: в окне у сессии всего четыре
// пробы, доля квантуется шагом 25%, и медиана таких долей — это шум, а не
// измерение. Сумма по девяти сессиям даёт около 36 проб, то есть шаг ~3%.
func (m *pathMonitor) snapshot() pathSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-pathStatsTTL)
	rtts := make([]float64, 0, len(m.samples))
	totalSent, totalPonged := 0, 0
	sessions := 0
	for id, s := range m.samples {
		if s.at.Before(cutoff) {
			delete(m.samples, id)
			continue
		}
		sessions++
		totalSent += s.sent
		totalPonged += s.ponged
		if s.haveRTT {
			rtts = append(rtts, s.rttMs)
		}
	}

	snap := pathSnapshot{probes: totalSent, sessions: sessions}
	if len(rtts) > 0 {
		snap.rttMs = median(rtts)
		snap.haveRTT = true
	}
	if totalSent > 0 {
		snap.loss = 1 - float64(totalPonged)/float64(totalSent)
		if snap.loss < 0 {
			snap.loss = 0
		}
	}
	return snap
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]float64(nil), v...)
	// Вставками: значений максимум по числу воркеров (единицы), заводить
	// sort.Float64s ради этого незачем.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

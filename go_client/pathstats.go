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
	rttMs float64
	loss  float64
	at    time.Time
}

type pathMonitor struct {
	mu      sync.Mutex
	samples map[int]pathSample
}

var globalPath = &pathMonitor{samples: make(map[int]pathSample)}

// report кладёт свежий замер от сессии sessionID.
func (m *pathMonitor) report(sessionID int, rttMs, loss float64) {
	m.mu.Lock()
	m.samples[sessionID] = pathSample{rttMs: rttMs, loss: loss, at: time.Now()}
	m.mu.Unlock()
}

// forget убирает замеры завершившейся сессии, чтобы они не участвовали в
// сводке после её смерти.
func (m *pathMonitor) forget(sessionID int) {
	m.mu.Lock()
	delete(m.samples, sessionID)
	m.mu.Unlock()
}

// snapshot отдаёт медианные RTT и потери по живым сессиям.
func (m *pathMonitor) snapshot() (rttMs, loss float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-pathStatsTTL)
	rtts := make([]float64, 0, len(m.samples))
	losses := make([]float64, 0, len(m.samples))
	for id, s := range m.samples {
		if s.at.Before(cutoff) {
			delete(m.samples, id)
			continue
		}
		rtts = append(rtts, s.rttMs)
		losses = append(losses, s.loss)
	}
	if len(rtts) == 0 {
		return 0, 0, false
	}
	return median(rtts), median(losses), true
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

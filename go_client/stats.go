package main

import (
	"log"
	"sync/atomic"
	"time"
)

type Stats struct {
	TotalBytesUp      atomic.Int64
	TotalBytesDown    atomic.Int64
	ActiveConnections atomic.Int32

	// Скорость туннеля за последнее окно — источник для обучения
	// адаптивной маскировки (см. updateRate/CurrentBps).
	rateBps        atomic.Int64
	rateBytes      atomic.Int64
	rateAtUnixNano atomic.Int64
}

func NewStats() *Stats {
	return &Stats{}
}

func (s *Stats) RunLoop(shutdown <-chan struct{}) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
			active := s.ActiveConnections.Load()
			up := s.TotalBytesUp.Load()
			down := s.TotalBytesDown.Load()
			totalMB := float64(up+down) / (1024.0 * 1024.0)
			upMB := float64(up) / (1024.0 * 1024.0)
			downMB := float64(down) / (1024.0 * 1024.0)

			// Скорость за окно — тот же счётчик, что и в статистике. Её
			// читает обучение адаптивной маскировки: без неё награда не
			// зависела от скорости вообще (см. aiobfs.Observe).
			s.updateRate(up + down)

			log.Printf("[СТАТИСТИКА] Активных: %d | Трафик: %.2f МБ | ↓%.2f МБ / ↑%.2f МБ", active, totalMB, downMB, upMB)
		}
	}
}

// updateRate пересчитывает текущую скорость туннеля по суммарным счётчикам.
// Вызывается из одного места (RunLoop), читается многими — отсюда atomic.
func (s *Stats) updateRate(totalBytes int64) {
	now := time.Now()
	prevAt := s.rateAtUnixNano.Swap(now.UnixNano())
	prevBytes := s.rateBytes.Swap(totalBytes)
	if prevAt == 0 {
		return
	}
	elapsed := float64(now.UnixNano()-prevAt) / 1e9
	if elapsed <= 0 {
		return
	}
	delta := totalBytes - prevBytes
	if delta < 0 {
		delta = 0
	}
	s.rateBps.Store(int64(float64(delta) * 8 / elapsed))
}

// CurrentBps — скорость туннеля в битах в секунду за последнее окно.
func (s *Stats) CurrentBps() float64 {
	return float64(s.rateBps.Load())
}

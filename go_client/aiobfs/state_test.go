package aiobfs

import (
	"testing"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// Выученное переживает перезапуск: веса после восстановления те же.
func TestStateRoundTrip(t *testing.T) {
	trained, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Учим так, чтобы веса заведомо разъехались.
	for i := 0; i < 200; i++ {
		trained.Observe(40, 0, 20e6)
		trained.Observe(280, 0.4, 1e6)
	}
	before := trained.Stats().BanditWeights

	blob, err := trained.ExportState()
	if err != nil {
		t.Fatalf("ExportState: %v", err)
	}

	fresh, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New (fresh): %v", err)
	}
	applied, err := fresh.ImportState(blob)
	if err != nil {
		t.Fatalf("ImportState: %v", err)
	}
	if !applied {
		t.Fatalf("состояние не применилось")
	}

	after := fresh.Stats().BanditWeights
	if len(before) != len(after) {
		t.Fatalf("разное число рук: %d против %d", len(before), len(after))
	}
	for i := range before {
		if diff := before[i] - after[i]; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("вес руки %d не восстановился: %g против %g", i, before[i], after[i])
		}
	}
}

// Несовместимый или повреждённый слепок игнорируется молча: начать с нуля
// всегда безопасно, падать из-за файла на диске — нет.
func TestStateRejectsIncompatible(t *testing.T) {
	s, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := map[string][]byte{
		"пусто":             nil,
		"другая версия":     []byte(`{"v":999,"n":5,"bandit":[1,1,1,1,1]}`),
		"другое число рук":  []byte(`{"v":1,"n":2,"bandit":[1,1]}`),
		"отрицательный вес": []byte(`{"v":1,"n":5,"bandit":[1,-1,1,1,1]}`),
	}
	for name, blob := range cases {
		applied, err := s.ImportState(blob)
		if err != nil {
			t.Fatalf("%s: неожиданная ошибка %v", name, err)
		}
		if applied {
			t.Fatalf("%s: слепок принят, хотя не должен", name)
		}
	}

	if _, err := s.ImportState([]byte("не json")); err == nil {
		t.Fatalf("битый JSON должен давать ошибку разбора")
	}
}

// Награда должна зависеть от скорости. Раньше член за скорость был
// константой (цель не задавалась), и обучение оптимизировало только RTT и
// потери — то есть могло предпочесть тихий, но медленный профиль.
func TestRewardSeesThroughput(t *testing.T) {
	fast, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	slow, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Одинаковые RTT и потери, разная скорость.
	for i := 0; i < 50; i++ {
		fast.Observe(50, 0, 50e6)
		slow.Observe(50, 0, 50e6)
	}
	for i := 0; i < 50; i++ {
		fast.Observe(50, 0, 50e6)
		slow.Observe(50, 0, 1e6)
	}

	sumFast, sumSlow := 0.0, 0.0
	for _, w := range fast.Stats().BanditWeights {
		sumFast += w
	}
	for _, w := range slow.Stats().BanditWeights {
		sumSlow += w
	}
	if sumFast == sumSlow {
		t.Fatalf("скорость не влияет на обучение: суммы весов совпали (%g)", sumFast)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wg-turn-client/aiobfs"
)

func newTestShaper(t *testing.T) *aiobfs.Shaper {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	s, err := aiobfs.New(aiobfs.Config{Key: key})
	if err != nil {
		t.Fatalf("aiobfs.New: %v", err)
	}
	return s
}

// Выученное переживает перезапуск процесса.
func TestAIStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	initAIState(dir, "lte-beeline")
	t.Cleanup(func() { globalAIState = nil })

	trained := newTestShaper(t)
	for i := 0; i < 100; i++ {
		trained.Observe(40, 0, 20e6)
		trained.Observe(290, 0.5, 1e6)
	}
	globalAIState.persist(trained)

	restored := newTestShaper(t)
	globalAIState.restore(restored)

	a := trained.Stats().BanditWeights
	b := restored.Stats().BanditWeights
	for i := range a {
		if diff := a[i] - b[i]; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("вес %d не восстановился: %g против %g", i, a[i], b[i])
		}
	}
}

// Опыт разных сетей не смешивается: у домашнего Wi-Fi и у соты фильтры
// разные, и общий файл учил бы среднее по больнице.
func TestAIStateSeparatesNetworks(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { globalAIState = nil })

	initAIState(dir, "wifi")
	wifiShaper := newTestShaper(t)
	for i := 0; i < 100; i++ {
		wifiShaper.Observe(20, 0, 90e6)
	}
	globalAIState.persist(wifiShaper)

	initAIState(dir, "lte-t2")
	if _, err := os.Stat(globalAIState.path); !os.IsNotExist(err) {
		t.Fatalf("память другой сети видна как своя: %s", globalAIState.path)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("ожидался ровно один файл памяти, найдено %d", len(entries))
	}
}

// Метка сети приходит снаружи — она не должна уводить запись из каталога.
func TestAITagCannotEscapeDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { globalAIState = nil })

	for _, tag := range []string{"../../etc/passwd", "a/b/c", "", "  ", strings.Repeat("x", 200)} {
		initAIState(dir, tag)
		if globalAIState == nil {
			t.Fatalf("хранилище не создано для метки %q", tag)
		}
		if got := filepath.Dir(globalAIState.path); got != dir {
			t.Fatalf("метка %q увела файл из каталога: %s", tag, globalAIState.path)
		}
	}
}

// Битый файл не должен ронять клиент: начать с нуля безопасно.
func TestAIStateToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	initAIState(dir, "lte")
	t.Cleanup(func() { globalAIState = nil })

	if err := os.WriteFile(globalAIState.path, []byte("{битый"), 0o600); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}
	globalAIState.restore(newTestShaper(t)) // не должно паниковать
}

// Право записи получает ровно одна сессия из девяти.
func TestAIStateSingleWriter(t *testing.T) {
	dir := t.TempDir()
	initAIState(dir, "lte")
	t.Cleanup(func() { globalAIState = nil })

	shaper := newTestShaper(t)
	owners := 0
	for i := 0; i < 9; i++ {
		if globalAIState.claimOwnership(shaper) {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("писателей должно быть ровно 1, получилось %d", owners)
	}

	globalAIState.releaseOwnership()
	if !globalAIState.claimOwnership(shaper) {
		t.Fatalf("после освобождения право записи не выдаётся")
	}
}

// Смена сети без перезапуска ядра: накопленное уходит в файл СТАРОЙ сети, а
// в живой шейпер подтягивается память НОВОЙ.
//
// Так бывает при выключенном экране: приложение намеренно не перезапускает
// туннель из-за фоновых событий сети, и без этой команды опыт мобильной сети
// писался бы в файл домашнего Wi-Fi, портя обе памяти.
func TestRetagSwitchesMemoryBetweenNetworks(t *testing.T) {
	dir := t.TempDir()
	initAIState(dir, "wifi")
	t.Cleanup(func() { globalAIState = nil })

	// Готовим память соты заранее — она должна подтянуться при переключении.
	cellShaper := newTestShaper(t)
	for i := 0; i < 200; i++ {
		cellShaper.Observe(30, 0, 50e6)
	}
	cellBlob, err := cellShaper.ExportState()
	if err != nil {
		t.Fatalf("ExportState: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aiobfs-cell-beeline.json"), cellBlob, 0o600); err != nil {
		t.Fatalf("подготовка файла соты: %v", err)
	}

	// Учимся в Wi-Fi.
	live := newTestShaper(t)
	globalAIState.claimOwnership(live)
	for i := 0; i < 200; i++ {
		live.Observe(200, 0.3, 1e6)
	}
	wifiWeights := append([]float64(nil), live.Stats().BanditWeights...)

	retagAIState("cell-beeline")

	// 1. Файл Wi-Fi создан и содержит то, что выучено в Wi-Fi.
	wifiPath := filepath.Join(dir, "aiobfs-wifi.json")
	if _, err := os.Stat(wifiPath); err != nil {
		t.Fatalf("память Wi-Fi не сохранена перед переключением: %v", err)
	}

	// 2. Живой шейпер теперь несёт веса соты, а не Wi-Fi.
	after := live.Stats().BanditWeights
	same := true
	for i := range after {
		if diff := after[i] - wifiWeights[i]; diff > 1e-9 || diff < -1e-9 {
			same = false
			break
		}
	}
	if same {
		t.Fatalf("после смены сети шейпер остался с весами прошлой сети")
	}

	// 3. Дальнейшая запись идёт в файл соты.
	globalAIState.persist(live)
	if got := filepath.Base(globalAIState.currentPath()); got != "aiobfs-cell-beeline.json" {
		t.Fatalf("память пишется не в ту сеть: %s", got)
	}
}

// Повторная метка той же сети ничего не делает: лишние чтения-записи файла
// на каждое событие сети ни к чему.
func TestRetagIgnoresSameNetwork(t *testing.T) {
	dir := t.TempDir()
	initAIState(dir, "wifi")
	t.Cleanup(func() { globalAIState = nil })

	before := globalAIState.currentPath()
	retagAIState("wifi")
	if globalAIState.currentPath() != before {
		t.Fatalf("путь изменился при той же метке")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("повторная метка создала файлы на пустом месте: %d", len(entries))
	}
}

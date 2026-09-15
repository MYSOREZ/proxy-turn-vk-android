package main

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wg-turn-client/aiobfs"
)

// aistate.go — память адаптивной маскировки между запусками.
//
// До этого весь онлайн-опыт жил ровно до конца процесса: каждое
// переподключение начинало с равномерных весов и заново перебирало профили,
// включая те, которые в этой сети заведомо душатся. Самое ценное в этой
// конструкции — как раз накопленное «в этой сети такая маскировка проходит,
// а такая нет», и оно терялось.
//
// Состояние раскладывается ПО СЕТЯМ: у домашнего Wi-Fi и у соты оператора
// разные фильтры, и смешивать их опыт — значит учить среднее по больнице.
// Метку сети даёт Android (-ai-net-tag), она безразмерная: тип транспорта
// плюс имя оператора, без SSID и без чего-либо, что указывает на место.
//
// В файле лежат только веса. Ни ключей, ни адресов, ни объёмов трафика.

const (
	aiStateSaveInterval = 60 * time.Second
	aiStateFileMode     = 0o600
	aiStateDirMode      = 0o700
)

type aiStateStore struct {
	// mu защищает путь и ссылку на шейпер владельца: метку сети может
	// сменить stdin-команда прямо во время работы (см. retag).
	mu   sync.Mutex
	dir  string
	tag  string
	path string
	// ownerShaper — шейпер той сессии, которая пишет файл. Нужен, чтобы при
	// смене сети сохранить накопленное в СТАРЫЙ файл и подтянуть память
	// новой сети в тот же живой шейпер.
	ownerShaper *aiobfs.Shaper
	// owner: состояние учит каждая сессия своим шейпером, а файл один.
	// Пишет первая захватившая флаг — остальные только читают при старте.
	owner atomic.Bool
}

func (s *aiStateStore) currentPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

var globalAIState *aiStateStore

var aiTagUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// initAIState готовит хранилище. Пустой dir полностью выключает память.
func initAIState(dir, tag string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, aiStateDirMode); err != nil {
		log.Printf("[AI-OBFS] Память выключена: не создать %s: %v", dir, err)
		return
	}
	safe := sanitizeAITag(tag)
	globalAIState = &aiStateStore{
		dir:  dir,
		tag:  safe,
		path: filepath.Join(dir, "aiobfs-"+safe+".json"),
	}
}

// sanitizeAITag не даёт метке сети превратиться в путь: она приходит снаружи.
func sanitizeAITag(tag string) string {
	tag = aiTagUnsafe.ReplaceAllString(strings.TrimSpace(tag), "-")
	tag = strings.Trim(tag, "-.")
	if tag == "" {
		return "default"
	}
	if len(tag) > 48 {
		tag = tag[:48]
	}
	return tag
}

// restore подмешивает выученное в новый шейпер.
func (s *aiStateStore) restore(shaper *aiobfs.Shaper) {
	if s == nil {
		return
	}
	path := s.currentPath()
	unlock := aiobfs.LockState()
	data, err := os.ReadFile(path)
	unlock()
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[AI-OBFS] Не прочитать память %s: %v", path, err)
		}
		return
	}
	applied, err := shaper.ImportState(data)
	switch {
	case err != nil:
		log.Printf("[AI-OBFS] Память повреждена, начинаю с нуля: %v", err)
	case applied:
		log.Printf("[AI-OBFS] Память восстановлена: %s", filepath.Base(path))
	default:
		log.Printf("[AI-OBFS] Память от другой версии профилей, начинаю с нуля")
	}
}

// persist записывает выученное. Пишем через временный файл: обрыв питания на
// телефоне — обычное дело, и получить вместо памяти половину файла не хочется.
func (s *aiStateStore) persist(shaper *aiobfs.Shaper) {
	if s == nil {
		return
	}
	data, err := shaper.ExportState()
	if err != nil {
		log.Printf("[AI-OBFS] Не сохранить память: %v", err)
		return
	}

	unlock := aiobfs.LockState()
	defer unlock()

	path := s.currentPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, aiStateFileMode); err != nil {
		log.Printf("[AI-OBFS] Не записать память: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[AI-OBFS] Не заменить файл памяти: %v", err)
		_ = os.Remove(tmp)
	}
}

// claimOwnership отдаёт право записи ровно одной сессии и запоминает её
// шейпер: при смене сети менять память надо именно в нём.
func (s *aiStateStore) claimOwnership(shaper *aiobfs.Shaper) bool {
	if s == nil || !s.owner.CompareAndSwap(false, true) {
		return false
	}
	s.mu.Lock()
	s.ownerShaper = shaper
	s.mu.Unlock()
	return true
}

func (s *aiStateStore) releaseOwnership() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ownerShaper = nil
	s.mu.Unlock()
	s.owner.Store(false)
}

// retag переключает память на другую сеть без перезапуска ядра.
//
// Зачем это отдельной командой. Обычно смена сети перезапускает ядро, и метка
// приезжает флагом. Но при выключенном экране приложение НАМЕРЕННО не
// перезапускает туннель из-за фоновых событий сети — и тогда опыт мобильной
// сети писался бы в файл домашнего Wi-Fi, портя обе памяти.
func retagAIState(tag string) {
	s := globalAIState
	if s == nil {
		return
	}
	safe := sanitizeAITag(tag)

	s.mu.Lock()
	if safe == s.tag {
		s.mu.Unlock()
		return
	}
	shaper := s.ownerShaper
	oldTag := s.tag
	s.mu.Unlock()

	// Сначала дописываем накопленное в файл СТАРОЙ сети.
	if shaper != nil {
		s.persist(shaper)
	}

	s.mu.Lock()
	s.tag = safe
	s.path = filepath.Join(s.dir, "aiobfs-"+safe+".json")
	s.mu.Unlock()

	log.Printf("[AI-OBFS] Сеть сменилась: %s -> %s, переключаю память", oldTag, safe)

	// И подтягиваем память новой сети в тот же живой шейпер.
	if shaper != nil {
		s.restore(shaper)
	}
}

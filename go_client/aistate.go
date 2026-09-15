package main

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	path string
	// owner: состояние учит каждая сессия своим шейпером, а файл один.
	// Пишет первая захватившая флаг — остальные только читают при старте.
	owner atomic.Bool
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
	globalAIState = &aiStateStore{path: filepath.Join(dir, "aiobfs-"+sanitizeAITag(tag)+".json")}
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
	unlock := aiobfs.LockState()
	data, err := os.ReadFile(s.path)
	unlock()
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[AI-OBFS] Не прочитать память %s: %v", s.path, err)
		}
		return
	}
	applied, err := shaper.ImportState(data)
	switch {
	case err != nil:
		log.Printf("[AI-OBFS] Память повреждена, начинаю с нуля: %v", err)
	case applied:
		log.Printf("[AI-OBFS] Память восстановлена: %s", filepath.Base(s.path))
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

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, aiStateFileMode); err != nil {
		log.Printf("[AI-OBFS] Не записать память: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[AI-OBFS] Не заменить файл памяти: %v", err)
		_ = os.Remove(tmp)
	}
}

// claimOwnership отдаёт право записи ровно одной сессии.
func (s *aiStateStore) claimOwnership() bool {
	return s != nil && s.owner.CompareAndSwap(false, true)
}

func (s *aiStateStore) releaseOwnership() {
	if s != nil {
		s.owner.Store(false)
	}
}

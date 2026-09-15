package main

import (
	"context"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const workersPerGroup = 9

const allocateGateInterval = 100 * time.Millisecond

// WorkerGroup:
// Запускает 9 потоков с одними кредами. Ротации нет — работает до смерти воркеров.
func WorkerGroup(
	ctx context.Context,
	groupID int,
	hashIndex int,
	tp *TurnParams,
	peer *net.UDPAddr,
	d *Dispatcher,
	localPort string,
	getConfig bool,
	configCh chan<- string,
	workerIDs []int,
	pauseFlag *int32,
	deviceID, password string,
	stats *Stats,
	waitReady <-chan struct{},
	signalReady chan<- struct{},
) {
	// Каскадный запуск: ждем свою очередь
	if waitReady != nil {
		log.Printf("[ГРУППА #%d] Ожидание сигнала от предыдущей группы...", groupID)
		select {
		case <-waitReady:
		case <-ctx.Done():
			return
		}
	}

	var configSent int32
	if !getConfig {
		configSent = 1
	}

	// Doze-mode пауза
	for atomic.LoadInt32(pauseFlag) != 0 {
		if ctx.Err() != nil {
			return
		}
		time.Sleep(1 * time.Second)
	}

	hash := tp.Hashes[hashIndex%len(tp.Hashes)]
	shortHash := hash
	if len(shortHash) > 8 {
		shortHash = shortHash[:8]
	}
	log.Printf("[ГРУППА #%d] Запрос кредов (хеш: %s...)", groupID, shortHash)

	credStreamID := groupID * 100
	user, pass, turnURLs, err := GetCreds(ctx, hash, credStreamID)
	var creds *Credentials
	if err == nil {
		creds = &Credentials{User: user, Pass: pass, TurnURLs: turnURLs, CacheStreamID: credStreamID}
	} else {
		log.Printf("[ГРУППА #%d] Ошибка кредов: %v", groupID, err)
		return
	}

	log.Printf("[ГРУППА #%d] Креды OK, TURN: %v, %d воркеров", groupID, creds.TurnURLs, len(workerIDs))

	var configRequestInFlight int32
	var wg sync.WaitGroup
	var credsMu sync.RWMutex
	var refreshMu sync.Mutex
	var lastCredRefresh atomic.Int64

	refreshCreds := func(reason string) bool {
		refreshMu.Lock()
		defer refreshMu.Unlock()

		now := time.Now().Unix()
		last := lastCredRefresh.Load()
		if last > 0 && now-last < 15 {
			log.Printf("[TURN] Креды уже обновлялись %d сек назад, ждём следующий retry (%s)", now-last, reason)
			return true
		}

		getStreamCache(credStreamID).invalidate(credStreamID)
		if getVkAuthMode() == "account" {
			invalidateInjectedTurnCreds(hash)
		}
		u, p, urls, refreshErr := GetCreds(ctx, hash, credStreamID)
		if refreshErr != nil {
			log.Printf("[TURN] Не удалось обновить креды после %s: %v", reason, refreshErr)
			return false
		}

		credsMu.Lock()
		creds = &Credentials{User: u, Pass: p, TurnURLs: urls, CacheStreamID: credStreamID}
		credsMu.Unlock()
		lastCredRefresh.Store(time.Now().Unix())
		log.Printf("[TURN] Креды обновлены после %s, TURN urls=%d", reason, len(urls))
		return true
	}

	// Сигнализируем следующей группе, что мы успешно запустились (креды получены + фора)
	if signalReady != nil {
		go func() {
			delayMs := 500 + rand.Intn(250)
			time.Sleep(time.Duration(delayMs) * time.Millisecond)
			close(signalReady)
			log.Printf("[ГРУППА #%d] Успешный старт! Передача эстафеты следующей группе...", groupID)
		}()
	}

	// Общий rate-limit на TURN Allocate по всей группе: не более одной новой
	// аллокации за тик, независимо от того, сколько воркеров сейчас готовы
	// её выполнить (стартовый stagger — отдельная вещь, см. workerDelay ниже —
	// он размазывает старт горутин, но не сами ретраи Allocate внутри уже
	// запущенных). Без этого на нестабильной сети несколько воркеров всё
	// равно накладываются друг на друга и вместе выжигают VK-квоту (error
	// 486) быстрее, чем должны. См. RunSession(allocateGate) в session.go и
	// комментарий там про free-turn-proxy — тот же приём.
	allocateTicker := time.NewTicker(allocateGateInterval)
	defer allocateTicker.Stop()

	for i, wid := range workerIDs {
		wg.Add(1)

		workerDelay := time.Duration(i) * 75 * time.Millisecond

		go func(wid int, delay time.Duration) {
			defer wg.Done()

			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}

			shouldGetConfig := getConfig
			attempt := 0

			for {
				if ctx.Err() != nil {
					return
				}

				getConf := false
				if shouldGetConfig && atomic.LoadInt32(&configSent) == 0 {
					getConf = atomic.CompareAndSwapInt32(&configRequestInFlight, 0, 1)
				}
				var cc chan<- string
				if getConf {
					cc = configCh
				}

				credsMu.RLock()
				credsSnapshot := *creds
				credsSnapshot.TurnURLs = cloneStringSlice(creds.TurnURLs)
				credsMu.RUnlock()

				configDelivered, sessErr := RunSession(ctx, tp, peer, d, localPort,
					getConf, cc, wid, &credsSnapshot, deviceID, password, stats, allocateTicker.C)

				quotaRetry := false
				fastRetry := false
				transientNetRetry := false
				if getConf {
					if configDelivered {
						atomic.StoreInt32(&configSent, 1)
					} else {
						atomic.StoreInt32(&configRequestInFlight, 0)
					}
				}

				if sessErr == nil {
					// Сессия отработала штатно — обнуляем счётчик попыток,
					// иначе нарастающая пауза копилась бы за всё время жизни
					// воркера и после одной ночной паузы он возвращался бы
					// минутами вместо секунд.
					attempt = 0
				}

				if sessErr != nil {
					if ctx.Err() != nil {
						return
					}
					errStr := sessErr.Error()
					errStrLower := strings.ToLower(errStr)
					fastRetry = strings.Contains(errStrLower, "broken pipe") ||
						strings.Contains(errStrLower, "connection reset by peer") ||
						strings.Contains(errStrLower, "unexpected eof")

					turnAllocAttrMissing := strings.Contains(errStrLower, "turn allocate") &&
						strings.Contains(errStrLower, "attribute not found")
					isTurnQuota := strings.Contains(errStrLower, "quota") || strings.Contains(errStr, "486")
					quotaRetry = isTurnQuota
					turnCredRefreshNeeded := !isTurnQuota && (turnAllocAttrMissing ||
						strings.Contains(errStrLower, "turn allocate auth") ||
						strings.Contains(errStrLower, "invalid credential") ||
						strings.Contains(errStrLower, "stale nonce") ||
						strings.Contains(errStrLower, "allocation mismatch") ||
						strings.Contains(errStrLower, "error 508"))

					if hint := workerErrorHint(sessErr); hint != "" {
						errStr += " | " + hint
					} else if strings.Contains(errStrLower, "rate limit") ||
						strings.Contains(errStrLower, "flood control") ||
						strings.Contains(errStrLower, "ip mismatch") ||
						strings.Contains(errStrLower, "error 29") {
						errStr += " (ошибка со стороны ВК)"
					}

					if strings.Contains(errStr, "хеш мёртв") ||
						strings.Contains(errStr, "FATAL_AUTH") {
						log.Printf("[ВОРКЕР #%d] Фатальная ошибка: %s", wid, errStr)
						return
					}

					attempt++
					if isTurnQuota {
						log.Printf("[ВОРКЕР #%d] [TURN] Квота relay исчерпана (один аккаунт VK = мало слотов), ждём: %s", wid, errStr)
					} else if turnAllocAttrMissing {
						log.Printf("[ВОРКЕР #%d] [TURN] Allocate вернул неполный ответ, обновляем TURN-креды и повторяем (попытка %d): %s", wid, attempt, errStr)
						refreshCreds("TURN Allocate attribute-not-found")
					} else if turnCredRefreshNeeded {
						log.Printf("[ВОРКЕР #%d] [TURN] Ошибка allocation/кредов, обновляем TURN-креды и повторяем (попытка %d): %s", wid, attempt, errStr)
						refreshCreds("TURN allocation error")
					} else {
						log.Printf("[ВОРКЕР #%d] Ошибка (попытка %d): %s", wid, attempt, errStr)
					}

					// Раньше "cannot create socket" и "error 29" считались
					// невосстановимыми, и воркер выходил из цикла НАВСЕГДА. На
					// телефоне это неверно: обе ошибки транзиентные.
					//
					// Во время дозы Android и при смене сети создание сокета
					// отказывает, потому что у процесса в этот момент нет сети,
					// а не потому, что что-то сломано насовсем. Воркер, поймавший
					// такой момент, больше не поднимался — отсюда в логах 6 из 9
					// и 8 из 9 активных после серии пробуждений, то есть потеря
					// трети пропускной способности релея до перезапуска туннеля.
					//
					// Теперь такие ошибки уводят воркера в длинную паузу с
					// нарастанием, но не убивают: проснулся телефон — воркер
					// вернулся сам.
					if isTransientNetworkError(errStrLower) {
						transientNetRetry = true
						log.Printf("[ВОРКЕР #%d] Сеть недоступна (попытка %d), ждём и повторяем: %s", wid, attempt, errStr)
					}
				}

				if ctx.Err() != nil {
					return
				}

				retryDelay := time.Duration(5+rand.Intn(11)) * time.Second
				switch {
				case quotaRetry:
					retryDelay = time.Duration(30+rand.Intn(31)) * time.Second
				case fastRetry:
					retryDelay = time.Duration(1+rand.Intn(3)) * time.Second
				case transientNetRetry:
					// Нарастающая пауза: телефон может спать долго, и долбиться
					// в отсутствующую сеть каждые 10 секунд — это только расход
					// батареи. Потолок в 5 минут, чтобы после пробуждения
					// воркер вернулся за разумное время.
					retryDelay = transientBackoff(attempt)
				}
				select {
				case <-time.After(retryDelay):
				case <-ctx.Done():
					return
				}
			}
		}(wid, workerDelay)
	}

	wg.Wait()
	log.Printf("[ГРУППА #%d] Все воркеры группы завершились.", groupID)
}

// ParseHashes — парсит строку хешей
func ParseHashes(raw string) []string {
	var result []string
	seen := make(map[string]struct{})
	for _, h := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		h = normalizeVKJoinHash(h)
		if h != "" {
			if _, exists := seen[h]; exists {
				continue
			}
			seen[h] = struct{}{}
			result = append(result, h)
		}
	}
	return result
}

func normalizeVKJoinHash(input string) string {
	s := strings.Trim(strings.TrimSpace(input), "<>\"'")
	if s == "" {
		return ""
	}

	lower := strings.ToLower(s)
	if idx := strings.Index(lower, "/call/join/"); idx >= 0 {
		s = s[idx+len("/call/join/"):]
	} else if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return ""
	}

	if idx := strings.IndexAny(s, "?#/"); idx != -1 {
		s = s[:idx]
	}
	return strings.Trim(strings.TrimSpace(s), "/")
}

// TurnParams — конфигурация TURN
type TurnParams struct {
	Host     string
	Port     string
	Hashes   []string
	WrapKey  []byte // Password-derived WRAP key (32 bytes), nil = disabled
	ObfsMode string // "audio" or "video" — RTP masking mode
	// NoDTLS: пропустить DTLS и идти RTP-obfs AEAD напрямую поверх TURN relay.
	// Требует сервер, который умеет принимать прямые (без DTLS) сессии на
	// отдельном порту/слушателе — см. server/main.go -listen-direct.
	NoDTLS bool
	// RawMode: raw-IP без WireGuard (см. server/main.go -listen-raw, handleConnRaw).
	// Подразумевает NoDTLS — сервер на -listen-raw DTLS не понимает.
	RawMode bool
	// AIObfs переключает RunSession со статичной обфускации
	// (ObfsConfig/ObfsState, obfsWrapPacket/obfsUnwrapPacket) на адаптивный
	// слой aiobfs: несколько профилей маскировки и онлайн-обучение вместо
	// одной формы на всю сессию. Сервер должен слушать -ai-listen, и peer
	// должен указывать именно на этот порт: форматы на проводе у двух слоёв
	// не совместимы (AES-256-GCM с RTP-заголовком как nonce против
	// ChaCha20-Poly1305 с другим порядком nonce). RunPing не затрагивает.
	AIObfs bool
	// TCPTransport: соединяться с TURN-relay по TCP вместо UDP (см.
	// dialTURNConn в session.go). На некоторых сетях (замечено на
	// Ростелекоме) UDP до TURN душится/дропается провайдером агрессивнее,
	// чем TCP на тот же relay — этот флаг обходит именно это.
	TCPTransport bool
}

// Credentials — учетные данные TURN
type Credentials struct {
	User          string
	Pass          string
	TurnURLs      []string
	CacheStreamID int
}

// transientBackoff — пауза перед повтором, когда у процесса просто нет сети
// (доза Android, переключение между сотами и Wi-Fi). Нарастает, чтобы не
// жечь батарею стуком в пустоту, но упирается в потолок, чтобы после
// пробуждения воркер вернулся за разумное время, а не через полчаса.
func transientBackoff(attempt int) time.Duration {
	const (
		base = 15 * time.Second
		max  = 5 * time.Minute
	)
	d := base
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	// Разброс, чтобы девять воркеров не ломились в сеть одной секундой.
	return d + time.Duration(rand.Intn(5000))*time.Millisecond
}

// isTransientNetworkError отличает «у процесса сейчас нет сети» от настоящей
// поломки. Такие ошибки приходят во время дозы Android и при переключении
// между сотами и Wi-Fi: сокет не создаётся, потому что сети нет, а не потому
// что что-то сломалось насовсем.
func isTransientNetworkError(errStrLower string) bool {
	for _, marker := range []string{
		"cannot create socket",
		"network is unreachable",
		"operation not permitted",
		"no route to host",
		"error 29",
	} {
		if strings.Contains(errStrLower, marker) {
			return true
		}
	}
	return false
}

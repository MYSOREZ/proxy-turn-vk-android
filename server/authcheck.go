package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// authcheck.go — внешняя авторизация Hysteria2 (`auth.type: command`).
//
// Зачем: пароль Hysteria2 нельзя держать отдельной константой. В qWDTT
// действующих паролей может быть несколько — пароль владельца из вкладки
// «Серверы» и пароли устройств, выданные через админ-панель или бота. Тоннель
// (WRAP) принимает любой из них, а Hysteria2, настроенная на один пароль,
// отвечала на остальные редиректом маскарада (HTTP 301) — со стороны клиента
// это выглядело как «authentication error» уже после успешного DTLS.
//
// Поэтому Hysteria2 спрашивает про пароль этот же бинарник:
//
//	hysteria → /usr/local/bin/wdtt-hy2-auth <addr> <пароль> <tx>
//	         → wdtt-hy2-server -config-dir … -password-file … -auth-check <пароль>
//
// Контракт hysteria (extras/auth.CommandAuthenticator): код возврата 0 —
// пустить, первая строка stdout — идентификатор пользователя. Любой другой
// код — отказ. Поэтому в stdout идёт ТОЛЬКО идентификатор, а диагностика — в
// stderr.
//
// Проверка read-only: база не открывается на запись и не перечитывается
// сервисом, так что параллельный запуск на каждое подключение безопасен.

// runAuthCheck возвращает код возврата процесса: 0 — пароль принят.
func runAuthCheck(configDir, mainPassword, candidate string) int {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		fmt.Fprintln(os.Stderr, "[AUTH] пустой пароль")
		return 1
	}

	if mainPassword != "" && candidate == mainPassword {
		fmt.Println("owner")
		return 0
	}

	path := filepath.Join(configDir, "passwords.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[AUTH] %s: %v\n", path, err)
		}
		return 1
	}

	var stored Database
	if err := json.Unmarshal(data, &stored); err != nil {
		fmt.Fprintf(os.Stderr, "[AUTH] повреждён %s: %v\n", path, err)
		return 1
	}

	entry, ok := stored.Passwords[candidate]
	if !ok || entry == nil || entry.IsDeactivated || isPasswordExpired(entry) {
		return 1
	}

	id := strings.TrimSpace(entry.Label)
	if id == "" {
		id = strings.TrimSpace(entry.DeviceID)
	}
	if id == "" {
		id = "user"
	}
	fmt.Println(id)
	return 0
}

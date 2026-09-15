package main

import (
	"strings"
	"testing"
	"time"
)

// Регрессия на «6 из 9 активных».
//
// Раньше "cannot create socket" и "error 29" считались невосстановимыми, и
// воркер выходил из цикла навсегда. На телефоне это неверно: обе ошибки
// приходят во время дозы Android и при смене сети, то есть означают «сети
// сейчас нет», а не «сломалось». Воркер, поймавший такой момент, больше не
// возвращался, и туннель терял треть релеев до перезапуска.
func TestTransientNetworkErrorsAreNotFatal(t *testing.T) {
	transient := []string{
		"turn allocate: cannot create socket",
		"dial udp: network is unreachable",
		"socket: operation not permitted",
		"connect: no route to host",
		"vk error 29: rate limit",
	}
	for _, e := range transient {
		if !isTransientNetworkError(strings.ToLower(e)) {
			t.Fatalf("ошибка признана фатальной, воркер не вернётся после сна: %q", e)
		}
	}

	permanent := []string{
		"FATAL_AUTH: неверный пароль подключения",
		"хеш мёртв",
		"turn allocate auth: invalid credential",
	}
	for _, e := range permanent {
		if isTransientNetworkError(strings.ToLower(e)) {
			t.Fatalf("настоящая ошибка принята за временную: %q", e)
		}
	}
}

// Пауза нарастает, но упирается в потолок: телефон может спать долго, а
// вернуться воркер должен за разумное время после пробуждения.
func TestTransientBackoffGrowsAndCaps(t *testing.T) {
	first := transientBackoff(1)
	if first < 15*time.Second || first > 21*time.Second {
		t.Fatalf("первая пауза вне ожидаемого диапазона: %v", first)
	}

	if transientBackoff(3) <= transientBackoff(1) {
		t.Fatalf("пауза не нарастает: %v -> %v", transientBackoff(1), transientBackoff(3))
	}

	for _, attempt := range []int{10, 50, 1000} {
		d := transientBackoff(attempt)
		if d > 5*time.Minute+5*time.Second {
			t.Fatalf("пауза пробила потолок на попытке %d: %v", attempt, d)
		}
	}
}

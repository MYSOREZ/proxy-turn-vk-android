package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/armon/go-socks5"
)

// hysteria.go — режим `-mode hysteria`.
//
// Идея: транспорт до VPS (TURN + DTLS + RTP-обфускация) остаётся ровно тем
// же, что и в обычном режиме. Меняются только концы туннеля:
//
//	было:  WireGuard  → 127.0.0.1:<listen> → TURN → VPS → встроенный WG
//	стало: Hysteria2  → 127.0.0.1:<listen> → TURN → VPS → сервер Hysteria2
//
// go_client уже слушает UDP на -listen и гонит всё, что туда придёт, через
// TURN. Протокол внутри он не разбирает, поэтому клиенту Hysteria2
// достаточно направить свой QUIC на этот локальный порт — про TURN он
// ничего не знает.
//
// Зачем это нужно: WireGuard — это L3-туннель без ретрансмита, потери
// TURN-релея проходят насквозь и их разгребает TCP внутри туннеля, схлопывая
// окно. QUIC у Hysteria2 восстанавливает потери сам и имеет собственный
// контроль перегрузки, поэтому внутренний TCP этих потерь не видит.

// fqdnKey переносит исходное доменное имя из резолвера в диалер, чтобы имя
// резолвил сервер Hysteria2, а не телефон (иначе был бы DNS-leak мимо
// туннеля).
type fqdnKey struct{}

// remoteResolver ничего не резолвит локально: он лишь запоминает имя и
// отдаёт заглушку, а настоящий резолвинг делает уже выходная нода.
type remoteResolver struct{}

func (remoteResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	return context.WithValue(ctx, fqdnKey{}, name), net.IPv4zero, nil
}

// HysteriaParams — всё, что нужно для подключения к серверной части.
type HysteriaParams struct {
	ServerAddr string // куда слать QUIC; обычно 127.0.0.1:<listen>, т.е. вход в TURN-туннель
	Auth       string // пароль Hysteria2
	SNI        string // имя в TLS SNI
	Insecure   bool   // не проверять сертификат (деплой ставит самоподписанный)
	UpMbps     uint64 // 0 → BBR; >0 → включается Brutal с этой полосой
	DownMbps   uint64
}

func mbpsToBps(mbps uint64) uint64 { return mbps * 1000 * 1000 / 8 }

// newHysteriaClient поднимает QUIC-сессию до сервера Hysteria2.
func newHysteriaClient(p HysteriaParams) (hyclient.Client, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", p.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("hysteria: разбор адреса %q: %w", p.ServerAddr, err)
	}

	cfg := &hyclient.Config{
		ServerAddr: udpAddr,
		Auth:       p.Auth,
		TLSConfig: hyclient.TLSConfig{
			ServerName:         p.SNI,
			InsecureSkipVerify: p.Insecure,
		},
	}
	// Ненулевая полоса включает Brutal — он шлёт с заданной скоростью и не
	// принимает потери за сигнал перегрузки. На душимом канале (а TURN-релей
	// именно такой) это обычно и даёт стабильность. Ноль оставляет BBR.
	if p.UpMbps > 0 || p.DownMbps > 0 {
		cfg.BandwidthConfig = hyclient.BandwidthConfig{
			MaxTx: mbpsToBps(p.UpMbps),
			MaxRx: mbpsToBps(p.DownMbps),
		}
	}

	c, info, err := hyclient.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("hysteria: подключение к %s: %w", p.ServerAddr, err)
	}
	if info != nil {
		// Tx == 0 означает, что сервер выбрал BBR, а не Brutal.
		cc := "BBR"
		if info.Tx > 0 {
			cc = fmt.Sprintf("Brutal, tx=%d B/s", info.Tx)
		}
		log.Printf("[HY2] Сессия установлена (UDP=%v, congestion control: %s)", info.UDPEnabled, cc)
	}
	return c, nil
}

// hysteriaDial отдаёт go-socks5 диалер, который уводит соединения в QUIC.
// Если резолвер положил в контекст доменное имя — идём по имени, чтобы DNS
// разрешался на выходной ноде.
func hysteriaDial(c hyclient.Client) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("hysteria: неподдерживаемая сеть %q", network)
		}
		target := addr
		if name, ok := ctx.Value(fqdnKey{}).(string); ok && name != "" {
			if _, port, err := net.SplitHostPort(addr); err == nil {
				target = net.JoinHostPort(name, port)
			}
		}
		return c.TCP(target)
	}
}

// runHysteriaSocks поднимает локальный SOCKS5, который ходит наружу через
// Hysteria2. Возвращается, когда ctx отменён.
func runHysteriaSocks(ctx context.Context, p HysteriaParams, socksAddr string, authEnabled bool, username, password string) error {
	c, err := newHysteriaClient(p)
	if err != nil {
		return err
	}
	defer c.Close()
	context.AfterFunc(ctx, func() { _ = c.Close() })

	conf := &socks5.Config{
		Dial:     hysteriaDial(c),
		Resolver: remoteResolver{},
		Logger:   log.New(io.Writer(socksLogFilter{}), "", 0),
	}
	if authEnabled {
		conf.Credentials = socks5.StaticCredentials{username: password}
	}
	server, err := socks5.New(conf)
	if err != nil {
		return fmt.Errorf("hysteria: socks5: %w", err)
	}

	ln, err := net.Listen("tcp", socksAddr)
	if err != nil {
		return fmt.Errorf("hysteria: слушатель SOCKS5 %s: %w", socksAddr, err)
	}
	defer ln.Close()
	context.AfterFunc(ctx, func() { _ = ln.Close() })

	log.Printf("[HY2] SOCKS5 на %s → Hysteria2 %s (SNI=%s, insecure=%v)", socksAddr, p.ServerAddr, p.SNI, p.Insecure)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Временная ошибка приёма — не роняем весь режим.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go func(nc net.Conn) {
			if err := server.ServeConn(nc); err != nil {
				log.Printf("[HY2] SOCKS5 сессия: %v", err)
			}
		}(conn)
	}
}

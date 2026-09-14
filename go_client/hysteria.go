package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
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

// ── Телеметрия туннеля ───────────────────────────────────────────────────────
//
// QUIC про TURN ничего не знает: он просто шлёт датаграммы в локальный порт.
// Если туннель не встал, клиент видит только "timeout: no recent network
// activity" — по нему нельзя понять, ушло ли хоть что-то и вернулось ли.
// Поэтому даём Hysteria2 обычный UDP-сокет, но со счётчиками, и раз в
// несколько секунд печатаем их, пока ответа нет.

type countingPacketConn struct {
	net.PacketConn
	sentPkts  atomic.Int64
	sentBytes atomic.Int64
	maxSent   atomic.Int64
	recvPkts  atomic.Int64
	recvBytes atomic.Int64
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *countingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n := int64(len(p))
	c.sentPkts.Add(1)
	c.sentBytes.Add(n)
	for {
		cur := c.maxSent.Load()
		if n <= cur || c.maxSent.CompareAndSwap(cur, n) {
			break
		}
	}
	return c.PacketConn.WriteTo(p, addr)
}

func (c *countingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.recvPkts.Add(1)
		c.recvBytes.Add(int64(n))
	}
	return n, addr, err
}

func (c *countingPacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.PacketConn.Close()
}

// watch печатает счётчики, пока из туннеля не пришёл первый пакет.
func (c *countingPacketConn) watch(serverAddr string) {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for i := 0; i < 5; i++ {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}
		recv := c.recvPkts.Load()
		log.Printf("[HY2] Туннель %s: отправлено %d пакетов (%d Б, макс %d Б), получено %d пакетов (%d Б)",
			serverAddr, c.sentPkts.Load(), c.sentBytes.Load(), c.maxSent.Load(), recv, c.recvBytes.Load())
		if recv > 0 {
			return
		}
		log.Printf("[HY2] Из туннеля не вернулось ни одного пакета. Проверьте, что порт сервера соответствует режиму: DTLS — 56100, «без DTLS» — 56102")
	}
}

// tunnelConnFactory — фабрика сокетов для Hysteria2 со счётчиками выше.
type tunnelConnFactory struct{}

func (tunnelConnFactory) New(addr net.Addr) (net.PacketConn, error) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	c := &countingPacketConn{PacketConn: pc, closed: make(chan struct{})}
	go c.watch(addr.String())
	return c, nil
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
		ConnFactory: tunnelConnFactory{},
		ServerAddr:  udpAddr,
		Auth:        p.Auth,
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
func runHysteriaSocks(ctx context.Context, p HysteriaParams, socksAddr string, authEnabled bool, username, password string, holder *hyClientHolder) error {
	c, err := newHysteriaClient(p)
	if err != nil {
		return err
	}
	defer c.Close()
	context.AfterFunc(ctx, func() { _ = c.Close() })

	// Тот же клиент обслуживает и системный VPN (см. hytun.go): сессия одна,
	// незачем держать вторую.
	if holder != nil {
		holder.set(c)
		defer holder.set(nil)
	}

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

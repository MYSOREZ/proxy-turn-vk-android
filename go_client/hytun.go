package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	hyerrs "github.com/apernet/hysteria/core/v2/errors"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// hytun.go — системный VPN для режима Hysteria2.
//
// Без него режим отдавал только локальный SOCKS5 на 127.0.0.1:1080: браузер и
// приложения о нём не знают, поэтому внешний IP не менялся. Теперь Android
// поднимает VpnService, отдаёт сюда TUN-дескриптор (тот же механизм, что и у
// raw-режима — TunFdBridge/SCM_RIGHTS), а мы разбираем IP-пакеты пользо-
// вательским стеком gvisor и превращаем их в соединения Hysteria2:
//
//	приложение → TUN → netstack → TCP: client.TCP(addr)
//	                            → UDP: client.UDP()  → QUIC → TURN → VPS
//
// Иначе говоря, это tun2socks, только вместо SOCKS5 — сразу Hysteria2, без
// лишнего локального хопа.
//
// Почему netstack, а не «сырые пакеты наружу», как в raw-режиме: Hysteria2 —
// проксирующий протокол (потоки и датаграммы), IP-пакеты в него не положить.
// Значит, TCP/IP надо терминировать на телефоне, что gvisor и делает.

const (
	hyTunAddr = "172.19.0.2" // адрес интерфейса; подсеть не пересекается с типовыми домашними
	hyTunMTU  = 1400
	// DNS, который Android пропишет интерфейсу. Запросы к ним уходят обычным
	// UDP в туннель, то есть резолвит их выходная нода, а не телефон —
	// утечки DNS мимо туннеля нет.
	hyTunDNS = "1.1.1.1,8.8.8.8"
	// Таймаут простоя UDP-сессии. DNS-ответ приходит за миллисекунды, а вот
	// QUIC/игровой трафик держится долго — 90с компромисс между утечкой
	// сессий и обрывом живых.
	hyUDPIdleTimeout = 90 * time.Second
)

// hyTunReadyMarker — строка, по которой Android понимает, что можно поднимать
// VpnService и слать TUN-дескриптор. Формат: маркер|IP|DNS|MTU.
func hyTunReadyMarker(dnsCSV string) string {
	return fmt.Sprintf("[HY2TUN] ГОТОВ|%s|%s|%d", hyTunAddr, dnsCSV, hyTunMTU)
}

// hyClientHolder отдаёт действующий клиент Hysteria2 и умеет ждать, пока тот
// поднимется: TUN появляется раньше, чем встанет QUIC через TURN, и пакеты в
// это время просто некуда девать.
type hyClientHolder struct {
	mu sync.RWMutex
	c  hyclient.Client
}

func (h *hyClientHolder) set(c hyclient.Client) {
	h.mu.Lock()
	h.c = c
	h.mu.Unlock()
}

func (h *hyClientHolder) get() hyclient.Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.c
}

// wait ждёт появления клиента не дольше d.
func (h *hyClientHolder) wait(ctx context.Context, d time.Duration) hyclient.Client {
	deadline := time.Now().Add(d)
	for {
		if c := h.get(); c != nil {
			return c
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runHysteriaTun поднимает netstack поверх TUN-дескриптора и гоняет через
// Hysteria2 весь трафик устройства. Возвращается по отмене ctx.
func runHysteriaTun(ctx context.Context, tunFile *os.File, holder *hyClientHolder) error {
	ep := channel.New(512, hyTunMTU, "")
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	})

	const nicID = tcpip.NICID(1)
	if err := s.CreateNIC(nicID, ep); err != nil {
		return fmt.Errorf("hytun: CreateNIC: %v", err)
	}
	// Promiscuous + spoofing: адреса назначения нам заранее неизвестны —
	// стек должен принимать пакеты на любой адрес и отвечать от его имени.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return fmt.Errorf("hytun: SetPromiscuousMode: %v", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return fmt.Errorf("hytun: SetSpoofing: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	tcpFwd := tcp.NewForwarder(s, 0, 2048, func(r *tcp.ForwarderRequest) {
		go handleHyTCP(ctx, r, holder)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	udpFwd := udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		go handleHyUDP(ctx, r, holder)
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	var wg sync.WaitGroup
	wg.Add(2)

	// TUN → стек
	go func() {
		defer wg.Done()
		buf := make([]byte, hyTunMTU+80)
		for ctx.Err() == nil {
			n, err := tunFile.Read(buf)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("[HY2TUN] Чтение TUN остановлено: %v", err)
				}
				return
			}
			if n == 0 {
				continue
			}
			var proto tcpip.NetworkProtocolNumber
			switch buf[0] >> 4 {
			case 4:
				proto = header.IPv4ProtocolNumber
			case 6:
				proto = header.IPv6ProtocolNumber
			default:
				continue
			}
			pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(buf[:n]),
			})
			ep.InjectInbound(proto, pkb)
			pkb.DecRef()
		}
	}()

	// стек → TUN
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			pkb := ep.ReadContext(ctx)
			if pkb == nil {
				return
			}
			view := pkb.ToView()
			pkb.DecRef()
			if _, err := tunFile.Write(view.AsSlice()); err != nil {
				view.Release()
				if ctx.Err() == nil {
					log.Printf("[HY2TUN] Запись в TUN остановлена: %v", err)
				}
				return
			}
			view.Release()
		}
	}()

	log.Printf("[HY2TUN] Стек поднят: %s/32, MTU %d — весь трафик устройства идёт в Hysteria2", hyTunAddr, hyTunMTU)
	<-ctx.Done()
	ep.Close()
	_ = tunFile.Close()
	wg.Wait()
	return nil
}

// handleHyTCP принимает TCP-соединение из стека и сращивает его с потоком
// Hysteria2 до того же адреса.
func handleHyTCP(ctx context.Context, r *tcp.ForwarderRequest, holder *hyClientHolder) {
	id := r.ID()
	target := net.JoinHostPort(addrString(id.LocalAddress), strconv.Itoa(int(id.LocalPort)))

	c := holder.wait(ctx, 20*time.Second)
	if c == nil {
		r.Complete(true) // RST: сессии Hysteria2 ещё нет
		return
	}
	remote, err := c.TCP(target)
	if err != nil {
		logHyDialFailure("TCP", target, err)
		r.Complete(true)
		return
	}

	var wq waiter.Queue
	tcpEP, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		remote.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(&wq, tcpEP)

	go func() {
		defer local.Close()
		defer remote.Close()
		_, _ = io.Copy(remote, local)
	}()
	go func() {
		defer local.Close()
		defer remote.Close()
		_, _ = io.Copy(local, remote)
	}()
}

// handleHyUDP отдаёт каждую UDP-сессию в свою датаграммную сессию Hysteria2.
func handleHyUDP(ctx context.Context, r *udp.ForwarderRequest, holder *hyClientHolder) {
	id := r.ID()
	target := net.JoinHostPort(addrString(id.LocalAddress), strconv.Itoa(int(id.LocalPort)))

	c := holder.wait(ctx, 20*time.Second)
	if c == nil {
		return
	}

	var wq waiter.Queue
	udpEP, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		return
	}
	local := gonet.NewUDPConn(&wq, udpEP)

	hyConn, err := c.UDP()
	if err != nil {
		logHyDialFailure("UDP", target, err)
		local.Close()
		return
	}

	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			local.Close()
			hyConn.Close()
		})
	}

	// приложение → Hysteria2
	go func() {
		defer closeBoth()
		buf := make([]byte, 65535)
		for {
			_ = local.SetReadDeadline(time.Now().Add(hyUDPIdleTimeout))
			n, err := local.Read(buf)
			if err != nil {
				return
			}
			if err := hyConn.Send(buf[:n], target); err != nil {
				return
			}
		}
	}()

	// Hysteria2 → приложение
	go func() {
		defer closeBoth()
		for {
			data, _, err := hyConn.Receive()
			if err != nil {
				return
			}
			_ = local.SetWriteDeadline(time.Now().Add(hyUDPIdleTimeout))
			if _, err := local.Write(data); err != nil {
				return
			}
		}
	}()

	go func() {
		<-ctx.Done()
		closeBoth()
	}()
}

// logHyDialFailure объясняет, ЧЬЯ это ошибка, и не даёт ей засорять лог.
//
// hysteria отдаёт DialError, когда до адреса не смогла достучаться выходная
// нода: текст ошибки ("dial tcp4 …: i/o timeout") сформирован на VPS и просто
// доставлен нам. Это не поломка туннеля и не повод для подсказки про
// недоступный VPS — туннель в этот момент работает. Всё остальное (закрытая
// сессия, локальный сбой) — уже наша сторона.
//
// Один и тот же адрес обычно отваливается пачкой попыток подряд, поэтому одна
// строка на адрес в hyDialLogEvery.
var (
	hyDialLogMu   sync.Mutex
	hyDialLogSeen = map[string]time.Time{}
)

const hyDialLogEvery = 30 * time.Second

func logHyDialFailure(kind, target string, err error) {
	hyDialLogMu.Lock()
	last, ok := hyDialLogSeen[target]
	if ok && time.Since(last) < hyDialLogEvery {
		hyDialLogMu.Unlock()
		return
	}
	hyDialLogSeen[target] = time.Now()
	if len(hyDialLogSeen) > 256 {
		for k, t := range hyDialLogSeen {
			if time.Since(t) > hyDialLogEvery {
				delete(hyDialLogSeen, k)
			}
		}
	}
	hyDialLogMu.Unlock()

	var dialErr hyerrs.DialError
	if errors.As(err, &dialErr) {
		log.Printf("[HY2TUN] %s %s: выходная нода не смогла подключиться (%s). Туннель при этом работает.",
			kind, target, dialErr.Message)
		return
	}
	log.Printf("[HY2TUN] %s %s: %v", kind, target, err)
}

// addrString печатает адрес назначения так, как его ждёт Hysteria2 (строка
// хоста), без аллокаций net.IP.String на каждый пакет.
func addrString(a tcpip.Address) string {
	return net.IP(a.AsSlice()).String()
}

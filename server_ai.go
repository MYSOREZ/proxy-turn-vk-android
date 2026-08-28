package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"wg-turn-client/aiobfs"

	"github.com/pion/dtls/v3"
	dtlsnet "github.com/pion/dtls/v3/pkg/net"
	pionudp "github.com/pion/transport/v4/udp"
	"golang.zx2c4.com/wireguard/device"
)

// server_ai.go adds a second, fully independent DTLS server backed by
// aiobfs's adaptive traffic-masking layer instead of the fixed audio/video
// RTP disguise the primary -listen server uses (see listenWrapped/
// wrapPacketConn above). It is entirely additive and opt-in: nothing here
// runs unless -ai-listen is set, it never touches the existing wrapKeyStore
// wire format, and it fails independently of the primary listener.
//
// Why a separate listener rather than replacing the existing one: aiobfs
// uses AES-256-GCM with the RTP header itself as the AEAD nonce, while the
// existing obfs layer uses ChaCha20-Poly1305 with a differently-ordered
// nonce (see obfsBuildNonce) — the two are not byte-compatible on the
// wire. A client built against -obfs audio/video must keep talking to
// -listen; a client built with -ai-obfs must talk to -ai-listen. Both can
// run on the same server at once, sharing the same active passwords (via
// serverWrapKeys.Keys()) and the same WireGuard backend (handleConn).
const aiAutonomousInterval = 3 * time.Second

func startAIListener(ctx context.Context, aiListenAddr, wgEndpoint string, wgDev *device.Device, keys *wgKeys, cert tls.Certificate) {
	if aiListenAddr == "" {
		return
	}

	addr, err := net.ResolveUDPAddr("udp", aiListenAddr)
	if err != nil {
		log.Fatalf("[AI-WRAP] адрес: %v", err)
	}
	if serverWrapKeys.Count() == 0 {
		log.Fatalf("[AI-WRAP] нет активных паролей для WRAP")
	}

	aioListener, err := aioListenWrapped(ctx, addr)
	if err != nil {
		log.Fatalf("[AI-WRAP] %v", err)
	}

	listener, err := dtls.NewListenerWithOptions(aioListener,
		dtls.WithCertificates(cert),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)),
		dtls.WithMTU(1100),
	)
	if err != nil {
		log.Fatalf("[AI-DTLS] %v", err)
	}
	context.AfterFunc(ctx, func() { listener.Close() })

	log.Printf("   AI-DTLS: %s | WG: %s | adaptive traffic masking (opt-in, independent of -listen)", aiListenAddr, wgEndpoint)

	go func() {
		var wg sync.WaitGroup
		for {
			dtlsConn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					wg.Wait()
					return
				default:
				}
				continue
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				handleConn(ctx, c, wgEndpoint, wgDev, keys)
			}(dtlsConn)
		}
	}()
}

func aioListenWrapped(ctx context.Context, addr *net.UDPAddr) (dtlsnet.PacketListener, error) {
	inner, err := pionudp.Listen("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("ai-wrap: udp listen: %w", err)
	}
	return &aioWrapPacketListener{
		ctx:   ctx,
		inner: dtlsnet.PacketListenerFromListener(inner),
	}, nil
}

type aioWrapPacketListener struct {
	ctx   context.Context
	inner dtlsnet.PacketListener
}

func (l *aioWrapPacketListener) Accept() (net.PacketConn, net.Addr, error) {
	pc, addr, err := l.inner.Accept()
	if err != nil {
		return pc, addr, err
	}
	return &aioWrapPacketConn{ctx: l.ctx, inner: pc}, addr, nil
}

func (l *aioWrapPacketListener) Close() error   { return l.inner.Close() }
func (l *aioWrapPacketListener) Addr() net.Addr { return l.inner.Addr() }

type aioWrapPacketConn struct {
	ctx   context.Context
	inner net.PacketConn

	selected  int32
	authLog   int32
	shaper    *aiobfs.Shaper
	remoteFor net.Addr // set once selected, for RunAutonomous's send func
	stopAuto  func()
}

func (c *aioWrapPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	buf := make([]byte, len(p)+256) // headroom for header/tag/padding
	for {
		n, addr, err := c.inner.ReadFrom(buf)
		if err != nil {
			return 0, addr, err
		}
		raw := buf[:n]

		if atomic.LoadInt32(&c.selected) == 0 {
			key, payload, isDecoy, uErr := aiobfs.TryUnwrap(serverWrapKeys.Keys(), raw)
			if uErr != nil {
				if atomic.CompareAndSwapInt32(&c.authLog, 0, 1) {
					log.Printf("[AI-WRAP] Отказ: AEAD auth failed from %s (keys=%d)", addr.String(), serverWrapKeys.Count())
				}
				return 0, addr, uErr
			}
			shaper, sErr := aiobfs.New(aiobfs.Config{Key: key})
			if sErr != nil {
				return 0, addr, fmt.Errorf("ai-wrap: shaper init: %w", sErr)
			}
			c.shaper = shaper
			c.remoteFor = addr
			atomic.StoreInt32(&c.selected, 1)
			c.stopAuto = shaper.RunAutonomous(c.ctx, func(wire []byte) error {
				_, werr := c.inner.WriteTo(wire, c.remoteFor)
				return werr
			}, aiAutonomousInterval)
			if atomic.CompareAndSwapInt32(&c.authLog, 0, 1) {
				log.Printf("[AI-WRAP] OK: ключ выбран для %s (keys=%d)", addr.String(), serverWrapKeys.Count())
			}
			if isDecoy {
				continue // first packet was a probe/decoy, not real DTLS data: keep reading
			}
			if len(payload) > len(p) {
				return 0, addr, fmt.Errorf("ai-wrap: decrypted payload (%d bytes) exceeds read buffer (%d bytes)", len(payload), len(p))
			}
			return copy(p, payload), addr, nil
		}

		payload, isDecoy, uErr := c.shaper.Unwrap(raw)
		if uErr != nil {
			// Password may have rotated: re-verify across all active keys,
			// same fallback the legacy -listen path has.
			key, payload2, isDecoy2, uErr2 := aiobfs.TryUnwrap(serverWrapKeys.Keys(), raw)
			if uErr2 != nil {
				return 0, addr, fmt.Errorf("ai-wrap unwrap: %w", uErr)
			}
			shaper, sErr := aiobfs.New(aiobfs.Config{Key: key})
			if sErr != nil {
				return 0, addr, fmt.Errorf("ai-wrap: shaper init: %w", sErr)
			}
			if c.stopAuto != nil {
				c.stopAuto()
			}
			c.shaper = shaper
			c.stopAuto = shaper.RunAutonomous(c.ctx, func(wire []byte) error {
				_, werr := c.inner.WriteTo(wire, c.remoteFor)
				return werr
			}, aiAutonomousInterval)
			log.Printf("[AI-WRAP] Обновлен ключ на лету для %s (пароль изменился/обновился)", addr.String())
			payload, isDecoy = payload2, isDecoy2
		}

		if pong, ok := c.shaper.PendingPong(); ok {
			_, _ = c.inner.WriteTo(pong, addr)
		}
		if isDecoy {
			continue // decoy/probe/pong: absorbed here, never handed to DTLS above
		}
		if len(payload) > len(p) {
			return 0, addr, fmt.Errorf("ai-wrap: decrypted payload (%d bytes) exceeds read buffer (%d bytes)", len(payload), len(p))
		}
		return copy(p, payload), addr, nil
	}
}

func (c *aioWrapPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if atomic.LoadInt32(&c.selected) == 0 || c.shaper == nil {
		return 0, errors.New("ai-wrap: key not selected")
	}
	wrapped, err := c.shaper.Wrap(p)
	if err != nil {
		return 0, fmt.Errorf("ai-wrap wrap: %w", err)
	}
	if _, err := c.inner.WriteTo(wrapped, addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *aioWrapPacketConn) Close() error {
	if c.stopAuto != nil {
		c.stopAuto()
	}
	return c.inner.Close()
}
func (c *aioWrapPacketConn) LocalAddr() net.Addr                { return c.inner.LocalAddr() }
func (c *aioWrapPacketConn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *aioWrapPacketConn) SetReadDeadline(t time.Time) error  { return c.inner.SetReadDeadline(t) }
func (c *aioWrapPacketConn) SetWriteDeadline(t time.Time) error { return c.inner.SetWriteDeadline(t) }

package main

import (
	"context"
	"net"
	"testing"
	"time"

	"wg-turn-client/aiobfs"
)

// fakePacketConn is a minimal net.PacketConn double for aioWrapPacketConn
// tests: pushed() queues bytes for ReadFrom to return (simulating packets
// "arriving" from a peer), and every WriteTo call is captured on written
// for the test to inspect.
type fakePacketConn struct {
	peer    net.Addr
	inbound chan []byte
	written chan []byte
}

func newFakePacketConn(peer net.Addr) *fakePacketConn {
	return &fakePacketConn{peer: peer, inbound: make(chan []byte, 16), written: make(chan []byte, 16)}
}

func (f *fakePacketConn) push(b []byte) { f.inbound <- append([]byte(nil), b...) }

func (f *fakePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	b := <-f.inbound
	return copy(p, b), f.peer, nil
}

func (f *fakePacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	f.written <- append([]byte(nil), p...)
	return len(p), nil
}

func (f *fakePacketConn) Close() error                     { return nil }
func (f *fakePacketConn) LocalAddr() net.Addr              { return f.peer }
func (f *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (f *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

func withTestPassword(t *testing.T, password string) {
	t.Helper()
	if err := serverWrapKeys.SetPasswords(password, nil); err != nil {
		t.Fatalf("SetPasswords: %v", err)
	}
	t.Cleanup(func() {
		_ = serverWrapKeys.SetPasswords("", nil)
	})
}

func TestAioWrapPacketConnBootstrapAndRoundTrip(t *testing.T) {
	withTestPassword(t, "integration-test-password")

	derivedKey, err := deriveWrapKey("integration-test-password")
	if err != nil {
		t.Fatalf("deriveWrapKey: %v", err)
	}
	clientShaper, err := aiobfs.New(aiobfs.Config{Key: derivedKey})
	if err != nil {
		t.Fatalf("aiobfs.New (client side): %v", err)
	}

	peerAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	fake := newFakePacketConn(peerAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &aioWrapPacketConn{ctx: ctx, inner: fake}

	// First packet: bootstrap should identify the key and hand back the
	// decrypted payload.
	first, err := clientShaper.Wrap([]byte("dtls clienthello would go here"))
	if err != nil {
		t.Fatalf("client Wrap: %v", err)
	}
	fake.push(first)

	buf := make([]byte, 2048)
	n, addr, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom (bootstrap): %v", err)
	}
	if got := string(buf[:n]); got != "dtls clienthello would go here" {
		t.Fatalf("bootstrap payload = %q", got)
	}
	if addr.String() != peerAddr.String() {
		t.Fatalf("addr = %v, want %v", addr, peerAddr)
	}
	if conn.shaper == nil {
		t.Fatalf("expected conn.shaper to be set after successful bootstrap")
	}

	// Second, ordinary packet on the now-established connection.
	second, err := clientShaper.Wrap([]byte("second real packet"))
	if err != nil {
		t.Fatalf("client Wrap 2: %v", err)
	}
	fake.push(second)
	n, _, err = conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom (second): %v", err)
	}
	if got := string(buf[:n]); got != "second real packet" {
		t.Fatalf("second payload = %q", got)
	}

	// A non-data packet must be absorbed (never handed to the caller) —
	// push a probe (the real path's own decoy-shaped self-monitoring
	// packet) followed by a real one, and confirm a single ReadFrom call
	// skips straight past the probe to the real packet.
	probeWire, err := clientShaper.SendProbe()
	if err != nil {
		t.Fatalf("client SendProbe: %v", err)
	}
	third, err := clientShaper.Wrap([]byte("third real packet"))
	if err != nil {
		t.Fatalf("client Wrap 3: %v", err)
	}
	fake.push(probeWire)
	fake.push(third)

	n, _, err = conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom (after probe): %v", err)
	}
	if got := string(buf[:n]); got != "third real packet" {
		t.Fatalf("expected ReadFrom to skip the probe and return the next real packet, got %q", got)
	}

	// The probe should have produced a pong, written straight back on the
	// underlying conn — verify the client can authenticate/open it.
	select {
	case pong := <-fake.written:
		_, isDecoy, err := clientShaper.Unwrap(pong)
		if err != nil {
			t.Fatalf("client Unwrap(pong): %v", err)
		}
		if !isDecoy {
			t.Fatalf("expected the pong to be classified as non-data")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected a pong to have been written in response to the probe")
	}

	// WriteTo: server -> client direction.
	if _, err := conn.WriteTo([]byte("server says hi"), peerAddr); err != nil {
		t.Fatalf("conn.WriteTo: %v", err)
	}
	select {
	case wire := <-fake.written:
		payload, isDecoy, err := clientShaper.Unwrap(wire)
		if err != nil {
			t.Fatalf("client Unwrap(server packet): %v", err)
		}
		if isDecoy {
			t.Fatalf("server's real packet misclassified as decoy")
		}
		if string(payload) != "server says hi" {
			t.Fatalf("payload = %q", payload)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected conn.WriteTo to have written a packet")
	}
}

func TestAioWrapPacketConnRejectsUnknownKey(t *testing.T) {
	withTestPassword(t, "the-real-password")

	wrongKey, err := deriveWrapKey("a-different-password")
	if err != nil {
		t.Fatalf("deriveWrapKey: %v", err)
	}
	attacker, err := aiobfs.New(aiobfs.Config{Key: wrongKey})
	if err != nil {
		t.Fatalf("aiobfs.New: %v", err)
	}

	peerAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	fake := newFakePacketConn(peerAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &aioWrapPacketConn{ctx: ctx, inner: fake}

	wire, err := attacker.Wrap([]byte("nope"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	fake.push(wire)

	buf := make([]byte, 2048)
	if _, _, err := conn.ReadFrom(buf); err == nil {
		t.Fatalf("expected bootstrap to fail for a packet wrapped under an unknown key")
	}
}

func TestWrapKeyStoreKeysReturnsIndependentCopies(t *testing.T) {
	withTestPassword(t, "keys-accessor-test")
	keys := serverWrapKeys.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected 1 active key, got %d", len(keys))
	}
	keys[0][0] ^= 0xFF // mutate the copy
	again := serverWrapKeys.Keys()
	if again[0][0] == keys[0][0] {
		t.Fatalf("Keys() did not return an independent copy — mutating the returned slice affected the store")
	}
}

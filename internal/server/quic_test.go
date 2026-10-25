package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

func listenServer(t testing.TB, idleTimeout time.Duration) *Listener {
	t.Helper()
	tlsConf, err := SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	conf, err := QUICConfig(idleTimeout)
	if err != nil {
		t.Fatalf("QUIC config: %v", err)
	}
	listener, err := Listen("127.0.0.1:0", tlsConf, conf)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Release() })
	return listener
}

func TestQUICConfig(t *testing.T) {
	t.Parallel()
	conf, err := QUICConfig(7 * time.Second)
	if err != nil {
		t.Fatalf("QUICConfig: %v", err)
	}
	if conf.MaxIncomingStreams != 1 {
		t.Errorf("MaxIncomingStreams = %d, want 1", conf.MaxIncomingStreams)
	}
	if conf.MaxIncomingUniStreams >= 0 {
		t.Errorf("MaxIncomingUniStreams = %d, want negative (none)", conf.MaxIncomingUniStreams)
	}
	if conf.MaxIdleTimeout != 7*time.Second {
		t.Errorf("MaxIdleTimeout = %s, want 7s", conf.MaxIdleTimeout)
	}
	if conf.HandshakeIdleTimeout <= 0 {
		t.Errorf("HandshakeIdleTimeout = %s, want it set explicitly", conf.HandshakeIdleTimeout)
	}
	if conf.MaxStreamReceiveWindow == 0 || conf.MaxStreamReceiveWindow > 1<<20 ||
		conf.MaxConnectionReceiveWindow == 0 || conf.MaxConnectionReceiveWindow > 4<<20 {
		t.Errorf("windows = stream %d, connection %d, want small explicit values",
			conf.MaxStreamReceiveWindow, conf.MaxConnectionReceiveWindow)
	}
	if conf.InitialStreamReceiveWindow > conf.MaxStreamReceiveWindow ||
		conf.InitialConnectionReceiveWindow > conf.MaxConnectionReceiveWindow {
		t.Errorf("initial windows exceed the maximum: %+v", conf)
	}
}

func TestQUICConfigRejectsNonPositiveIdleTimeout(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Second} {
		if conf, err := QUICConfig(d); err == nil {
			t.Errorf("QUICConfig(%s) = %+v, want an error", d, conf)
		}
	}
}

func TestListenRejectsBadAddress(t *testing.T) {
	t.Parallel()
	tlsConf, err := SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	if l, err := Listen("not an address", tlsConf, nil); err == nil {
		_ = l.Release()
		t.Fatal("Listen on a bad address succeeded")
	}
	if l, err := Listen("127.0.0.1:0", nil, nil); err == nil {
		_ = l.Release()
		t.Fatal("Listen without TLS config succeeded")
	}
}

func TestListenerReleaseFreesSocketAndStopsAccept(t *testing.T) {
	t.Parallel()
	listener := listenServer(t, DefaultIdleTimeout)
	udpAddr, ok := listener.Addr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("Addr = %T, want *net.UDPAddr", listener.Addr())
	}

	if err := listener.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := listener.Accept(context.Background()); !errors.Is(err, quic.ErrServerClosed) {
		t.Errorf("Accept after close = %v, want ErrServerClosed", err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("socket still bound after close: %v", err)
	}
	_ = conn.Close()
}

// retryCounter counts the QUIC v1 Retry packets received (one per Initial packet).
type retryCounter struct {
	net.PacketConn
	retries atomic.Int32
}

func (c *retryCounter) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err == nil && n > 0 && p[0]&0x80 != 0 && (p[0]>>4)&0x03 == 0x03 {
		c.retries.Add(1)
	}
	return n, addr, err
}

func TestListenValidatesSourceAddressWithRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	listener := listenServer(t, DefaultIdleTimeout)
	go func() {
		conn, err := listener.Accept(ctx)
		if err == nil {
			_ = conn.CloseWithError(0, "")
		}
	}()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client socket: %v", err)
	}
	counter := &retryCounter{PacketConn: udp}
	transport := &quic.Transport{Conn: counter}
	t.Cleanup(func() { _ = transport.Close(); _ = udp.Close() })
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	conn, err := transport.Dial(ctx, listener.Addr(), tlsConf, &quic.Config{Versions: []quic.Version{quic.Version1}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseWithError(0, "") }()

	if counter.retries.Load() == 0 {
		t.Error("client never received a Retry packet")
	}
}

func TestSecondStreamIsRefused(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv := New(agg, discardLogger)
	listener := listenServer(t, DefaultIdleTimeout)
	client := dialQUIC(ctx, t, listener.Listener)
	done := runHandleConn(ctx, t, srv, listener.Listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	c := newTestClient(stream)
	sendAndExpectAck(t, c, "foo:1|c")

	var limitErr *quic.StreamLimitReachedError
	if second, err := client.OpenStream(); !errors.As(err, &limitErr) {
		t.Errorf("second bidirectional stream = %v, %v; want StreamLimitReachedError", second, err)
	}
	if uni, err := client.OpenUniStream(); !errors.As(err, &limitErr) {
		t.Errorf("unidirectional stream = %v, %v; want StreamLimitReachedError", uni, err)
	}
	sendAndExpectAck(t, c, "foo:2|c")

	if err := client.CloseWithError(0, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitDone(t, done)
}

func TestListenerCloseLeavesSessionsToTheServerShutdown(t *testing.T) {
	t.Parallel()
	for range 30 {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		agg, _ := newTestAggregator()
		srv := New(agg, discardLogger)
		listener := listenServer(t, DefaultIdleTimeout)
		serveCtx, stopServe := context.WithCancel(ctx)
		served := make(chan struct{})
		go func() {
			defer close(served)
			_ = srv.Serve(serveCtx, listener.Listener)
		}()
		client := dialQUIC(ctx, t, listener.Listener)
		stream, err := client.OpenStreamSync(ctx)
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		sendAndExpectAck(t, newTestClient(stream), "foo:1|c")

		stopServe()
		_ = listener.Close()
		waitDone(t, served)
		_ = listener.Release()

		select {
		case <-client.Context().Done():
		case <-ctx.Done():
			t.Fatal("client was never told the session closed")
		}
		var appErr *quic.ApplicationError
		if err := context.Cause(client.Context()); !errors.As(err, &appErr) || !appErr.Remote || appErr.ErrorCode != 0 {
			t.Fatalf("close cause = %v, want a clean remote application close", err)
		}
		cancel()
	}
}

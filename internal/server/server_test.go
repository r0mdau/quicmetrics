package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

const testTimeout = 5 * time.Second

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newCapturingServer(agg *Aggregator) (*Server, *syncBuffer) {
	var logs syncBuffer
	return New(agg, slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

func assertNoSessionPanic(t *testing.T, logs *syncBuffer) {
	t.Helper()
	if strings.Contains(logs.String(), "panic in session") {
		t.Errorf("handleConn recovered a panic:\n%s", logs.String())
	}
}

func listenQUIC(t testing.TB) *quic.Listener {
	t.Helper()
	tlsConf, err := SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConf, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func dialQUIC(ctx context.Context, t testing.TB, listener *quic.Listener) *quic.Conn {
	t.Helper()
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{protocol.ALPN},
		MinVersion:         tls.VersionTLS13,
	}
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), tlsConf, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "") })
	return conn
}

func runHandleConn(ctx context.Context, t *testing.T, srv *Server, listener *quic.Listener) <-chan struct{} {
	t.Helper()
	conn, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleConn(ctx, conn)
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the server to return")
	}
}

func echoOnce(stream *quic.Stream, msg string) error {
	if _, err := stream.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write %q: %w", msg, err)
	}
	echo := make([]byte, len(msg))
	if _, err := io.ReadFull(stream, echo); err != nil {
		return fmt.Errorf("read echo of %q: %w", msg, err)
	}
	if string(echo) != msg {
		return fmt.Errorf("echo = %q, want %q", echo, msg)
	}
	return nil
}

func sendAndExpectEcho(t *testing.T, stream *quic.Stream, msg string) {
	t.Helper()
	if err := echoOnce(stream, msg); err != nil {
		t.Fatal(err)
	}
}

func TestEchoWriterEchoesInput(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		"users.online:1|c|#country:france",
		"temperature:21.5|g",
		"not a metric",
		"",
		"foo:1|c|",
		"foo:1|g|",
		"foo:1|c||#a:b",
		"foo:1|c|#a:b|",
		"foo:1|c|@0.5|",
		"foo\xff:1|c",
	} {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()
			agg, _ := newTestAggregator()
			var buf bytes.Buffer
			n, err := echoWriter{ctx: context.Background(), w: &buf, agg: agg}.Write([]byte(msg))
			if err != nil || n != len(msg) {
				t.Fatalf("Write(%q) = %d, %v; want %d, nil", msg, n, err, len(msg))
			}
			if buf.String() != msg {
				t.Errorf("echo = %q, want %q", buf.String(), msg)
			}
		})
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestEchoWriterPropagatesEchoError(t *testing.T) {
	t.Parallel()
	agg, _ := newTestAggregator()
	want := errors.New("stream closed")

	_, err := echoWriter{ctx: context.Background(), w: failingWriter{want}, agg: agg}.Write([]byte("foo:1|c"))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestHandleConnSurvivesClientWithoutStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv, logs := newCapturingServer(agg)

	listener := listenQUIC(t)
	client := dialQUIC(ctx, t, listener)
	done := runHandleConn(ctx, t, srv, listener)

	if err := client.CloseWithError(0, "no stream"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, logs)
}

func TestHandleConnEchoesAndAggregatesOverQUIC(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, reader := newTestAggregator()
	srv, logs := newCapturingServer(agg)

	listener := listenQUIC(t)
	client := dialQUIC(ctx, t, listener)
	done := runHandleConn(ctx, t, srv, listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	for _, msg := range []string{"foo:1|c|", "foo:2|c|#a:x"} {
		sendAndExpectEcho(t, stream, msg)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, logs)

	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{a=x}": 2})
}

func TestHandleConnTreatsClientCloseAsClean(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv, logs := newCapturingServer(agg)

	listener := listenQUIC(t)
	client := dialQUIC(ctx, t, listener)
	done := runHandleConn(ctx, t, srv, listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendAndExpectEcho(t, stream, "foo:1|c")
	if err := client.CloseWithError(0, "done"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	waitDone(t, done)
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("clean client close was logged as a warning:\n%s", logs.String())
	}
}

func TestServeReturnsNilWhenListenerClosed(t *testing.T) {
	t.Parallel()
	agg, _ := newTestAggregator()
	srv := New(agg, discardLogger)
	listener := listenQUIC(t)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(context.Background(), listener) }()

	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Serve did not return after listener close")
	}
}

func TestServeClosesSessionsWhenContextCancelled(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	srv := New(agg, discardLogger)
	listener := listenQUIC(t)

	serveCtx, stopServe := context.WithCancel(context.Background())
	defer stopServe()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(serveCtx, listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	client := dialQUIC(ctx, t, listener)
	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendAndExpectEcho(t, stream, "foo:1|c")

	stopServe()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Serve did not return after cancel with an open session")
	}
	if err := stream.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Error("session still open after Serve returned")
	}
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 1})
}

func serveQUIC(ctx context.Context, tb testing.TB, srv *Server) *quic.Listener {
	tb.Helper()
	listener := listenQUIC(tb)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, listener)
	}()
	tb.Cleanup(func() { <-done })
	return listener
}

// BenchmarkSessionsParallel sends generated counters from several QUIC connections at once,
// each waiting for its echo before the next write like the real client.
func BenchmarkSessionsParallel(b *testing.B) {
	lines := benchLines(b, 1024)
	for _, conns := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("conns_%d", conns), func(b *testing.B) {
			ctx := b.Context()
			agg, reader := newTestAggregator()
			listener := serveQUIC(ctx, b, New(agg, discardLogger))
			streams := make([]*quic.Stream, conns)
			for i := range streams {
				stream, err := dialQUIC(ctx, b, listener).OpenStreamSync(ctx)
				if err != nil {
					b.Fatalf("open stream: %v", err)
				}
				if err := echoOnce(stream, lines[0]); err != nil {
					b.Fatal(err)
				}
				streams[i] = stream
			}

			var next atomic.Int64
			var wg sync.WaitGroup
			b.ResetTimer()
			for _, stream := range streams {
				wg.Go(func() {
					for i := next.Add(1); i <= int64(b.N); i = next.Add(1) {
						if err := echoOnce(stream, lines[int(i)%len(lines)]); err != nil {
							b.Error(err)
							return
						}
					}
				})
			}
			wg.Wait()
			b.StopTimer()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "metrics/s")

			if !b.Failed() {
				if got, want := sumValues(values(collectSums(b, reader))), float64(b.N+conns); got != want {
					b.Fatalf("recorded total = %v, want %v", got, want)
				}
			}
		})
	}
}

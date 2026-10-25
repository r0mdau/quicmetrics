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
	"testing/iotest"
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

func ackOnce(stream *quic.Stream, msg string) error {
	if _, err := stream.Write([]byte(msg + "\n")); err != nil {
		return fmt.Errorf("write %q: %w", msg, err)
	}
	return readAck(stream, msg)
}

func readAck(r io.Reader, want string) error {
	ack := make([]byte, len(want)+1)
	if _, err := io.ReadFull(r, ack); err != nil {
		return fmt.Errorf("read ack of %q: %w", want, err)
	}
	if string(ack) != want+"\n" {
		return fmt.Errorf("ack = %q, want %q", ack, want+"\n")
	}
	return nil
}

func sendAndExpectAck(t *testing.T, stream *quic.Stream, msg string) {
	t.Helper()
	if err := ackOnce(stream, msg); err != nil {
		t.Fatal(err)
	}
}

func serveBytes(t *testing.T, srv *Server, in io.Reader) (acks string, err error) {
	t.Helper()
	var out bytes.Buffer
	err = srv.serveStream(context.Background(), struct {
		io.Reader
		io.Writer
	}{in, &out})
	return out.String(), err
}

func TestServeStreamSplitsLines(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		in   string
		acks string
		want map[string]float64
	}{
		{"one line", "foo:1|c\n", "foo:1|c\n", map[string]float64{"foo{}": 1}},
		{"several lines", "foo:1|c\nfoo:2|c|#a:x\nfoo:3|c|#a:x\n", "foo:1|c\nfoo:2|c|#a:x\nfoo:3|c|#a:x\n", map[string]float64{"foo{}": 1, "foo{a=x}": 5}},
		{"CRLF", "foo:1|c\r\nfoo:2|c\r\n", "foo:1|c\nfoo:2|c\n", map[string]float64{"foo{}": 3}},
		{"empty lines", "\n\r\nfoo:1|c\n\n\n", "foo:1|c\n", map[string]float64{"foo{}": 1}},
		{"last line without terminator", "foo:1|c\nfoo:2|c", "foo:1|c\nfoo:2|c\n", map[string]float64{"foo{}": 3}},
		{"carriage return inside a line is kept", "foo:1|c\rbar\n", "foo:1|c\rbar\n", nil},
		{"invalid lines are acknowledged", "not a metric\nfoo:1|c|\nusers.unique:alice|s\nfoo:1|g|\nfoo:1|c||#a:b\nfoo:1|c|#a:b|\nfoo:1|c|@0.5|\nfoo\xff:1|c\n",
			"not a metric\nfoo:1|c|\nusers.unique:alice|s\nfoo:1|g|\nfoo:1|c||#a:b\nfoo:1|c|#a:b|\nfoo:1|c|@0.5|\nfoo\xff:1|c\n", nil},
		{"all metric types", "users.online:1|c|#country:france\ntemperature:21.5|g\nlatency:12|h\nlatency:12|ms\n",
			"users.online:1|c|#country:france\ntemperature:21.5|g\nlatency:12|h\nlatency:12|ms\n", map[string]float64{"users.online{country=france}": 1}},
		{"empty stream", "", "", nil},
	} {
		for _, oneByte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oneByte=%t", tt.name, oneByte), func(t *testing.T) {
				t.Parallel()
				agg, reader := newTestAggregator()
				var in io.Reader = strings.NewReader(tt.in)
				if oneByte {
					in = iotest.OneByteReader(in)
				}
				acks, err := serveBytes(t, New(agg, discardLogger), in)
				if err != nil {
					t.Fatalf("serveStream: %v", err)
				}
				if acks != tt.acks {
					t.Errorf("acks = %q, want %q", acks, tt.acks)
				}
				assertValues(t, values(collectSums(t, reader)), tt.want)
			})
		}
	}
}

func TestServeStreamLineLimit(t *testing.T) {
	t.Parallel()
	line := func(n int) string { return "foo:1|c|#t:" + strings.Repeat("x", n-len("foo:1|c|#t:")) }
	for _, tt := range []struct {
		name    string
		in      string
		wantErr bool
		acks    int
	}{
		{"max length", line(MaxLineLength) + "\n", false, 1},
		{"max length with CRLF", line(MaxLineLength) + "\r\n", false, 1},
		{"max length at EOF", line(MaxLineLength), false, 1},
		{"max length with CR at EOF", line(MaxLineLength) + "\r", false, 1},
		{"one byte too long", line(MaxLineLength+1) + "\n", true, 0},
		{"one byte too long with CRLF", line(MaxLineLength+1) + "\r\n", true, 0},
		{"one byte too long at EOF", line(MaxLineLength + 1), true, 0},
		{"without terminator", strings.Repeat("x", 4*MaxLineLength), true, 0},
		{"after valid lines", "foo:1|c\n" + line(MaxLineLength+1) + "\nfoo:1|c\n", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg, _ := newTestAggregator()
			acks, err := serveBytes(t, New(agg, discardLogger), strings.NewReader(tt.in))
			if errors.Is(err, errLineTooLong) != tt.wantErr || (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want line too long: %t", err, tt.wantErr)
			}
			if got := strings.Count(acks, "\n"); got != tt.acks {
				t.Errorf("%d acks, want %d", got, tt.acks)
			}
		})
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestServeStreamPropagatesAckError(t *testing.T) {
	t.Parallel()
	agg, _ := newTestAggregator()
	want := errors.New("stream closed")

	err := New(agg, discardLogger).serveStream(context.Background(), struct {
		io.Reader
		io.Writer
	}{strings.NewReader("foo:1|c\n"), failingWriter{want}})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestServeStreamPropagatesReadError(t *testing.T) {
	t.Parallel()
	agg, _ := newTestAggregator()
	want := errors.New("connection reset")

	acks, err := serveBytes(t, New(agg, discardLogger), io.MultiReader(strings.NewReader("foo:1|c\nfoo:2"), iotest.ErrReader(want)))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if acks != "foo:1|c\n" {
		t.Errorf("acks = %q, want only the complete line", acks)
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

func TestHandleConnAcknowledgesAndAggregatesOverQUIC(t *testing.T) {
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
		sendAndExpectAck(t, stream, msg)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, logs)

	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{a=x}": 2})
}

func TestHandleConnFramesLinesOverQUIC(t *testing.T) {
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
	if _, err := stream.Write([]byte("foo:1|c\nfoo:2|c|#a:x\nfoo:3|c")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, want := range []string{"foo:1|c", "foo:2|c|#a:x"} {
		if err := readAck(stream, want); err != nil {
			t.Fatal(err)
		}
	}
	for _, part := range []string{"|#a", ":x,b:", "y"} {
		if _, err := stream.Write([]byte(part)); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := stream.Write([]byte("\r\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := readAck(stream, "foo:3|c|#a:x,b:y"); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, logs)

	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 1, "foo{a=x}": 2, "foo{a=x,b=y}": 3})
}

func TestHandleConnClosesSessionOnLineTooLong(t *testing.T) {
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
	sendAndExpectAck(t, stream, "foo:1|c")
	var appErr *quic.ApplicationError
	_, err = stream.Write([]byte(strings.Repeat("x", 2*MaxLineLength)))
	if err != nil && (!errors.As(err, &appErr) || appErr.ErrorCode != errCodeLineTooLong) {
		t.Fatalf("write: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, logs)

	_, err = stream.Read(make([]byte, 1))
	if !errors.As(err, &appErr) || appErr.ErrorCode != errCodeLineTooLong {
		t.Errorf("read error = %v, want application error code %d", err, errCodeLineTooLong)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("oversized line was not logged as a warning:\n%s", logs.String())
	}
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 1})
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
	sendAndExpectAck(t, stream, "foo:1|c")
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
	sendAndExpectAck(t, stream, "foo:1|c")

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
// each waiting for its ack before the next write like the real client.
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
				if err := ackOnce(stream, lines[0]); err != nil {
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
						if err := ackOnce(stream, lines[int(i)%len(lines)]); err != nil {
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

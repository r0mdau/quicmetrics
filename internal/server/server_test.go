package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/quic-go/quic-go"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/r0mdau/quicmetrics/internal/protocol"
	"github.com/r0mdau/quicmetrics/internal/tlstest"
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

type testClient struct {
	stream *quic.Stream
	acks   *bufio.Reader
	sent   uint64
	acked  uint64
}

func newTestClient(stream *quic.Stream) *testClient {
	return &testClient{stream: stream, acks: bufio.NewReader(stream)}
}

func (c *testClient) write(lines ...string) error {
	if _, err := c.stream.Write([]byte(strings.Join(lines, "\n") + "\n")); err != nil {
		return fmt.Errorf("write %q: %w", lines, err)
	}
	c.sent += uint64(len(lines))
	return nil
}

func (c *testClient) writeRaw(data string, lines uint64) error {
	if _, err := c.stream.Write([]byte(data)); err != nil {
		return fmt.Errorf("write %q: %w", data, err)
	}
	c.sent += lines
	return nil
}

func (c *testClient) waitAcked() error {
	for c.acked < c.sent {
		if err := c.readOneAck(); err != nil {
			return err
		}
	}
	return nil
}

func (c *testClient) readOneAck() error {
	line, err := c.acks.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read ack (%d of %d processed): %w", c.acked, c.sent, err)
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(line, "\n"), 10, 64)
	if err != nil || n <= c.acked || n > c.sent {
		return fmt.Errorf("ack %q, want a count in (%d, %d]", line, c.acked, c.sent)
	}
	c.acked = n
	return nil
}

func (c *testClient) ackOnce(msg string) error {
	if err := c.write(msg); err != nil {
		return err
	}
	return c.waitAcked()
}

func sendAndExpectAck(t *testing.T, c *testClient, msg string) {
	t.Helper()
	if err := c.ackOnce(msg); err != nil {
		t.Fatal(err)
	}
}

func parseAcks(t *testing.T, out string) []uint64 {
	t.Helper()
	var acks []uint64
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" && out == "" {
			break
		}
		n, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			t.Fatalf("ack %q in %q: %v", line, out, err)
		}
		if len(acks) > 0 && n <= acks[len(acks)-1] {
			t.Fatalf("acks %q are not strictly increasing", out)
		}
		acks = append(acks, n)
	}
	return acks
}

type plainStream struct {
	io.Reader
	io.Writer
}

func (plainStream) SetReadDeadline(time.Time) error  { return nil }
func (plainStream) SetWriteDeadline(time.Time) error { return nil }

func serveBytes(t *testing.T, srv *Server, in io.Reader) (acks string, err error) {
	t.Helper()
	var out bytes.Buffer
	err = srv.serveStream(context.Background(), plainStream{in, &out})
	return out.String(), err
}

func TestServeStreamSplitsLines(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		in       string
		lines    uint64
		bulkAcks int
		want     map[string]float64
	}{
		{"one line", "foo:1|c\n", 1, 1, map[string]float64{"foo{}": 1}},
		{"several lines", "foo:1|c\nfoo:2|c|#a:x\nfoo:3|c|#a:x\n", 3, 1, map[string]float64{"foo{}": 1, "foo{a=x}": 5}},
		{"CRLF", "foo:1|c\r\nfoo:2|c\r\n", 2, 1, map[string]float64{"foo{}": 3}},
		{"empty lines", "\n\r\nfoo:1|c\n\n\n", 1, 1, map[string]float64{"foo{}": 1}},
		{"last line without terminator", "foo:1|c\nfoo:2|c", 2, 2, map[string]float64{"foo{}": 3}},
		{"carriage return inside a line is kept", "foo:1|c\rbar\n", 1, 1, nil},
		{"invalid lines are acknowledged", "not a metric\nfoo:1|c|\nusers.unique:alice|s\nfoo:1|g|\nfoo:1|c||#a:b\nfoo:1|c|#a:b|\nfoo:1|c|@0.5|\nfoo\xff:1|c\n", 8, 1, nil},
		{"all metric types", "users.online:1|c|#country:france\ntemperature:21.5|g\nlatency:12|h\nlatency:12|ms\n", 4, 1, map[string]float64{"users.online{country=france}": 1}},
		{"empty stream", "", 0, 0, nil},
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
				got := parseAcks(t, acks)
				wantAcks := uint64(tt.bulkAcks)
				if oneByte {
					wantAcks = tt.lines
				}
				if uint64(len(got)) != wantAcks || (tt.lines > 0 && got[len(got)-1] != tt.lines) {
					t.Errorf("acks = %v, want %d acks ending at %d", got, wantAcks, tt.lines)
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
		lines   uint64
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
			var last uint64
			if got := parseAcks(t, acks); len(got) > 0 {
				last = got[len(got)-1]
			}
			if last != tt.lines {
				t.Errorf("last ack = %d, want %d", last, tt.lines)
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

	err := New(agg, discardLogger).serveStream(context.Background(), plainStream{strings.NewReader("foo:1|c\n"), failingWriter{want}})
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
	if acks != "1\n" {
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
	c := newTestClient(stream)
	for _, msg := range []string{"foo:1|c|", "foo:2|c|#a:x"} {
		sendAndExpectAck(t, c, msg)
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
	c := newTestClient(stream)
	steps := []struct {
		data  string
		lines uint64
	}{
		{"foo:1|c\nfoo:2|c|#a:x\nfoo:3|c", 2},
		{"|#a", 0},
		{":x,b:", 0},
		{"y", 0},
		{"\r\n\n", 1},
	}
	for _, step := range steps {
		if err := c.writeRaw(step.data, step.lines); err != nil {
			t.Fatal(err)
		}
		if err := c.waitAcked(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
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
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	reader := sdkmetric.NewManualReader()
	agg := NewAggregator(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), logger)
	srv := New(agg, logger)

	listener := listenQUIC(t)
	client := dialQUIC(ctx, t, listener)
	done := runHandleConn(ctx, t, srv, listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendAndExpectAck(t, newTestClient(stream), "foo:1|c")
	var appErr *quic.ApplicationError
	_, err = stream.Write([]byte(strings.Repeat("x", 2*MaxLineLength)))
	if err != nil && (!errors.As(err, &appErr) || appErr.ErrorCode != protocol.CodeLineTooLong) {
		t.Fatalf("write: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, &logs)

	_, err = stream.Read(make([]byte, 1))
	if !errors.As(err, &appErr) || appErr.ErrorCode != protocol.CodeLineTooLong {
		t.Errorf("read error = %v, want application error code %d", err, protocol.CodeLineTooLong)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "reason=line_too_long") {
		t.Errorf("oversized line was not logged as a warning:\n%s", logs.String())
	}
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 1})
	assertDropped(t, reader, map[string]int64{"line_too_long": 1})
	if !strings.Contains(logs.String(), "example=127.0.0.1:") {
		t.Errorf("the drop does not name the client:\n%s", logs.String())
	}
}

func TestHandleConnAcknowledgesPipelinedBatchesInBulk(t *testing.T) {
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
	c := newTestClient(stream)
	const batches, perBatch = 20, 500
	for range batches {
		batch := make([]string, perBatch)
		for i := range batch {
			batch[i] = "foo:1|c|#a:x"
		}
		if err := c.write(batch...); err != nil {
			t.Fatal(err)
		}
	}
	var acks int
	for c.acked < c.sent {
		before := c.acked
		if err := c.readOneAck(); err != nil {
			t.Fatal(err)
		}
		acks++
		if c.acked <= before {
			t.Fatalf("ack went from %d to %d", before, c.acked)
		}
	}
	if acks >= batches*perBatch/10 {
		t.Errorf("%d acks for %d lines, want far fewer than one per line", acks, batches*perBatch)
	}
	if err := client.CloseWithError(0, "done"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	waitDone(t, done)
	assertNoSessionPanic(t, logs)

	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{a=x}": batches * perBatch})
}

func TestHandleConnLogsTheClientCertificateSubject(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	serverCA, clientCA := tlstest.NewCA(t, "server CA"), tlstest.NewCA(t, "client CA")
	file := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, file, clientCA.PEM)
	cas, _ := newTestCAReloader(t, file)
	listener := listenMTLS(t, serverCA, cas)
	agg, _ := newTestAggregator()
	srv, logs := newCapturingServer(agg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept(ctx)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		srv.handleConn(ctx, conn)
	}()
	client := clientCA.IssueClient(t, "agent-7")
	if err := exchangeWithClientCert(ctx, listener, serverCA, &client); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	waitDone(t, done)
	for _, want := range []string{"session accepted", "session closed", `client_subject="CN=agent-7"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
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
	sendAndExpectAck(t, newTestClient(stream), "foo:1|c")
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
	sendAndExpectAck(t, newTestClient(stream), "foo:1|c")

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

func TestServeRejectsSessionsBeyondTheCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, reader := newTestAggregator()
	var logs syncBuffer
	srv := New(agg, slog.New(slog.NewTextHandler(&logs, nil)), WithMaxSessions(1))
	listener := listenServer(t, DefaultIdleTimeout)
	serveListener(ctx, t, srv, listener)

	stream, err := dialQUIC(ctx, t, listener.Listener).OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	first := newTestClient(stream)
	sendAndExpectAck(t, first, "foo:1|c")

	rejected := dialQUIC(ctx, t, listener.Listener)
	select {
	case <-rejected.Context().Done():
	case <-ctx.Done():
		t.Fatal("connection beyond the cap was not closed")
	}
	var appErr *quic.ApplicationError
	if err := context.Cause(rejected.Context()); !errors.As(err, &appErr) ||
		!appErr.Remote || appErr.ErrorCode != protocol.CodeServerBusy || appErr.ErrorMessage == "" {
		t.Errorf("close cause = %v, want a remote application error %d with a reason", err, protocol.CodeServerBusy)
	}
	if !strings.Contains(logs.String(), "too many sessions") {
		t.Errorf("rejection was not logged:\n%s", logs.String())
	}

	sendAndExpectAck(t, first, "foo:2|c")
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 3})
}

func TestServeFreesTheSlotWhenASessionEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv := New(agg, discardLogger, WithMaxSessions(1))
	listener := listenServer(t, DefaultIdleTimeout)
	serveListener(ctx, t, srv, listener)

	client := dialQUIC(ctx, t, listener.Listener)
	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendAndExpectAck(t, newTestClient(stream), "foo:1|c")
	if err := client.CloseWithError(0, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}

	for {
		stream, err := dialQUIC(ctx, t, listener.Listener).OpenStreamSync(ctx)
		if err == nil && newTestClient(stream).ackOnce("foo:1|c") == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("slot of the closed session was never freed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestIdleSessionIsClosedQuietly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv, logs := newCapturingServer(agg)
	listener := listenServer(t, 500*time.Millisecond)
	client := dialQUIC(ctx, t, listener.Listener)
	done := runHandleConn(ctx, t, srv, listener.Listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendAndExpectAck(t, newTestClient(stream), "foo:1|c")
	start := time.Now()

	waitDone(t, done)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("idle session closed after %s, want about 500ms", elapsed)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("idle close was logged as a warning:\n%s", logs.String())
	}
}

// serveWithPipe runs serveStream on one end of a pipe, which has real deadlines and no buffer.
func serveWithPipe(t *testing.T, srv *Server) (client net.Conn, result <-chan error) {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() { _ = clientEnd.Close() })
	errc := make(chan error, 1)
	go func() {
		defer func() { _ = serverEnd.Close() }()
		errc <- srv.serveStream(context.Background(), serverEnd)
	}()
	return clientEnd, errc
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(testTimeout):
		t.Fatal("serveStream did not return")
		return nil
	}
}

func readAck(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != want {
		t.Fatalf("ack = %q (%v), want %q", got, err, want)
	}
}

func TestServeStreamClosesASessionThatSendsNoLine(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		client func(net.Conn)
	}{
		{"silence", func(net.Conn) {}},
		{"empty lines", func(c net.Conn) {
			for {
				if _, err := c.Write([]byte("\r\n")); err != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}},
		{"a line one byte at a time", func(c net.Conn) {
			for {
				if _, err := c.Write([]byte("f")); err != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg, reader := newTestAggregator()
			srv := New(agg, discardLogger, WithTimeouts(Timeouts{Line: 150 * time.Millisecond}))
			client, result := serveWithPipe(t, srv)
			go tt.client(client)

			start := time.Now()
			if err := waitResult(t, result); !errors.Is(err, errNoLine) {
				t.Fatalf("serveStream = %v, want %v", err, errNoLine)
			}
			if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
				t.Errorf("closed after %s, want about 150ms", elapsed)
			}
			assertValues(t, values(collectSums(t, reader)), nil)
		})
	}
}

func TestServeStreamKeepsASessionThatSendsLinesInTime(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	srv := New(agg, discardLogger, WithTimeouts(Timeouts{Line: 500 * time.Millisecond, Ack: time.Second}))
	client, result := serveWithPipe(t, srv)

	start := time.Now()
	for i := 1; time.Since(start) < 700*time.Millisecond; i++ {
		if _, err := client.Write([]byte("foo:1|c\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		readAck(t, client, strconv.Itoa(i)+"\n")
		time.Sleep(50 * time.Millisecond)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := waitResult(t, result); err != nil {
		t.Fatalf("serveStream = %v, want nil", err)
	}
	if got := values(collectSums(t, reader))["foo{}"]; got < 5 {
		t.Errorf("foo = %v, want at least 5 lines recorded", got)
	}
}

func TestServeStreamClosesASessionThatDoesNotTakeItsAck(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	srv := New(agg, discardLogger, WithTimeouts(Timeouts{Ack: 100 * time.Millisecond}))
	client, result := serveWithPipe(t, srv)

	if _, err := client.Write([]byte("foo:1|c\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := waitResult(t, result); !errors.Is(err, errAckNotRead) {
		t.Fatalf("serveStream = %v, want %v", err, errAckNotRead)
	}
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 1})
}

func TestServeStreamWithoutTimeoutsKeepsASlowSession(t *testing.T) {
	t.Parallel()
	agg, _ := newTestAggregator()
	srv := New(agg, discardLogger, WithTimeouts(Timeouts{}))
	client, result := serveWithPipe(t, srv)

	time.Sleep(300 * time.Millisecond)
	if _, err := client.Write([]byte("foo:1|c\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	readAck(t, client, "1\n")
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := waitResult(t, result); err != nil {
		t.Fatalf("serveStream = %v, want nil", err)
	}
}

func assertSlowClientClose(ctx context.Context, t *testing.T, conn *quic.Conn, reason string) {
	t.Helper()
	select {
	case <-conn.Context().Done():
	case <-ctx.Done():
		t.Fatal("session was not closed")
	}
	var appErr *quic.ApplicationError
	if err := context.Cause(conn.Context()); !errors.As(err, &appErr) ||
		!appErr.Remote || appErr.ErrorCode != protocol.CodeSlowClient || appErr.ErrorMessage != reason {
		t.Errorf("close cause = %v, want a remote application error %d with reason %q", err, protocol.CodeSlowClient, reason)
	}
}

func TestHandleConnClosesSessionThatOpensNoStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	reader := sdkmetric.NewManualReader()
	agg := NewAggregator(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), logger)
	srv := New(agg, logger, WithTimeouts(Timeouts{FirstStream: 100 * time.Millisecond}))

	listener := listenQUIC(t)
	client := dialQUIC(ctx, t, listener)
	done := runHandleConn(ctx, t, srv, listener)

	waitDone(t, done)
	assertSlowClientClose(ctx, t, client, "no stream opened")
	assertDropped(t, reader, map[string]int64{"slow_client": 1})
	for _, want := range []string{"level=WARN", "reason=slow_client", "example=127.0.0.1:"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

func TestHandleConnClosesSessionThatStopsSendingLines(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, reader := newTestAggregator()
	var logs syncBuffer
	srv := New(agg, slog.New(slog.NewTextHandler(&logs, nil)), WithTimeouts(Timeouts{Line: 200 * time.Millisecond}))

	listener := listenQUIC(t)
	client := dialQUIC(ctx, t, listener)
	done := runHandleConn(ctx, t, srv, listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendAndExpectAck(t, newTestClient(stream), "foo:1|c")
	go func() {
		for {
			if _, err := stream.Write([]byte("\n")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	waitDone(t, done)
	assertNoSessionPanic(t, &logs)
	assertSlowClientClose(ctx, t, client, "no line received")
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"foo{}": 1})
	assertDropped(t, reader, map[string]int64{"slow_client": 1})
}

func TestHandleConnClosesSessionThatNeverReadsItsAcks(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv := New(agg, discardLogger, WithTimeouts(Timeouts{Ack: 200 * time.Millisecond}))
	listener := listenQUIC(t)
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	client, err := quic.DialAddr(ctx, listener.Addr().String(), tlsConf, &quic.Config{
		InitialStreamReceiveWindow: 32,
		MaxStreamReceiveWindow:     32,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseWithError(0, "") })
	done := runHandleConn(ctx, t, srv, listener)

	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	go func() {
		for ctx.Err() == nil {
			if _, err := stream.Write([]byte("foo:1|c\n")); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	waitDone(t, done)
	assertSlowClientClose(ctx, t, client, "ack not read")
}

func TestHandleConnNamesTheCertificateOfASlowClient(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	serverCA, clientCA := tlstest.NewCA(t, "server CA"), tlstest.NewCA(t, "client CA")
	file := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, file, clientCA.PEM)
	cas, _ := newTestCAReloader(t, file)
	listener := listenMTLS(t, serverCA, cas)
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	reader := sdkmetric.NewManualReader()
	agg := NewAggregator(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), logger)
	srv := New(agg, logger, WithTimeouts(Timeouts{FirstStream: 100 * time.Millisecond}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept(ctx)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		srv.handleConn(ctx, conn)
	}()
	pair := clientCA.IssueClient(t, "agent-7")
	cert, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(serverCA.PEM)
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{
		RootCAs:              pool,
		NextProtos:           []string{protocol.ALPN},
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil },
	}, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "") })

	waitDone(t, done)
	if !strings.Contains(logs.String(), "CN=agent-7") || !strings.Contains(logs.String(), "reason=slow_client") {
		t.Errorf("the drop does not name the client certificate:\n%s", logs.String())
	}
}

func TestServeFreesTheSlotOfASlowSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agg, _ := newTestAggregator()
	srv := New(agg, discardLogger, WithMaxSessions(1), WithTimeouts(Timeouts{FirstStream: 200 * time.Millisecond}))
	listener := listenServer(t, DefaultIdleTimeout)
	serveListener(ctx, t, srv, listener)

	squatter := dialQUIC(ctx, t, listener.Listener)
	assertSlowClientClose(ctx, t, squatter, "no stream opened")

	for {
		stream, err := dialQUIC(ctx, t, listener.Listener).OpenStreamSync(ctx)
		if err == nil && newTestClient(stream).ackOnce("foo:1|c") == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("slot of the slow session was never freed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func serveListener(ctx context.Context, tb testing.TB, srv *Server, listener *Listener) {
	tb.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, listener.Listener)
	}()
	tb.Cleanup(func() { <-done })
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

// BenchmarkSessionsParallel pipelines batches of generated counters from several QUIC connections.
func BenchmarkSessionsParallel(b *testing.B) {
	const benchBatch, benchWindow = 100, 4
	lines := benchLines(b, 1024)
	for _, conns := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("conns_%d", conns), func(b *testing.B) {
			ctx := b.Context()
			agg, reader := newTestAggregator()
			listener := serveQUIC(ctx, b, New(agg, discardLogger))
			clients := make([]*testClient, conns)
			for i := range clients {
				stream, err := dialQUIC(ctx, b, listener).OpenStreamSync(ctx)
				if err != nil {
					b.Fatalf("open stream: %v", err)
				}
				clients[i] = newTestClient(stream)
				if err := clients[i].ackOnce(lines[0]); err != nil {
					b.Fatal(err)
				}
			}

			var next atomic.Int64
			var wg sync.WaitGroup
			b.ResetTimer()
			for _, c := range clients {
				wg.Go(func() {
					batch := make([]string, 0, benchBatch)
					for {
						start := int(next.Add(benchBatch)) - benchBatch
						if start >= b.N {
							break
						}
						batch = batch[:0]
						for i := start; i < min(start+benchBatch, b.N); i++ {
							batch = append(batch, lines[i%len(lines)])
						}
						if err := c.write(batch...); err != nil {
							b.Error(err)
							return
						}
						for c.sent-c.acked > benchWindow*benchBatch {
							if err := c.readOneAck(); err != nil {
								b.Error(err)
								return
							}
						}
					}
					if err := c.waitAcked(); err != nil {
						b.Error(err)
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

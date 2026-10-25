package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

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

var (
	quicAddrRe = regexp.MustCompile(`msg="serving QUIC metrics listener" addr=(\S+)`)
	httpAddrRe = regexp.MustCompile(`msg="serving Prometheus metrics" addr=(\S+)`)
)

func waitForAddr(t *testing.T, logs *syncBuffer, re *regexp.Regexp) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(logs.String()); m != nil {
			return m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never logged %s:\n%s", re, logs.String())
	return ""
}

func sendMetrics(t *testing.T, stream *quic.Stream, msgs ...string) {
	t.Helper()
	if _, err := stream.Write([]byte(strings.Join(msgs, "\n") + "\n")); err != nil {
		t.Fatalf("write %q: %v", msgs, err)
	}
	acks := bufio.NewReader(stream)
	for processed := 0; processed < len(msgs); {
		line, err := acks.ReadString('\n')
		if err != nil {
			t.Fatalf("read ack after %d of %d metrics: %v", processed, len(msgs), err)
		}
		n, err := strconv.Atoi(strings.TrimSuffix(line, "\n"))
		if err != nil || n <= processed || n > len(msgs) {
			t.Fatalf("ack %q, want a count in (%d, %d]", line, processed, len(msgs))
		}
		processed = n
	}
}

func TestRunServesMetricsAndShutsDownCleanly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0"}, &logs)
	}()
	quicAddr := waitForAddr(t, &logs, quicAddrRe)
	httpAddr := waitForAddr(t, &logs, httpAddrRe)

	dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDial()
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	conn, err := quic.DialAddr(dialCtx, quicAddr, tlsConf, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseWithError(0, "") }()
	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	sendMetrics(t, stream,
		"e2e.hits:4|c|#env:test",
		"a.b:1|c",
		"a_b:1|c",
		"process_cpu_seconds:1|c",
	)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + httpAddr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200 despite colliding metric names:\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{`e2e_hits_total{env="test"} 4`, "target_info", "go_goroutines", "promhttp_metric_handler_requests_total"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
	if strings.Contains(string(body), "otel_scope_") {
		t.Errorf("/metrics has scope labels:\n%s", body)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("run = %v, want nil after cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestRunFailsWhenMetricsPortIsTaken(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()

	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-metrics-addr", taken.Addr().String()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen HTTP") {
		t.Fatalf("run = %v, want a listen HTTP error", err)
	}
}

func TestRunFlags(t *testing.T) {
	t.Parallel()
	if err := run(context.Background(), []string{"-h"}, io.Discard); err != nil {
		t.Errorf("run -h = %v, want nil", err)
	}
	for _, args := range [][]string{{"-no-such-flag"}, {"-log-level", "loud"}} {
		if err := run(context.Background(), args, io.Discard); !errors.Is(err, errUsage) {
			t.Errorf("run %v = %v, want errUsage", args, err)
		}
	}
}

func TestRunRejectsInvalidQUICLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ flag, value string }{
		{"-max-sessions", "0"},
		{"-max-sessions", "-3"},
		{"-idle-timeout", "0s"},
		{"-idle-timeout", "-1s"},
	} {
		args := []string{tc.flag, tc.value}
		err := run(context.Background(), append([]string{"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0"}, args...), io.Discard)
		if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), tc.flag) {
			t.Errorf("run %v = %v, want a validation error naming %s", args, err, tc.flag)
		}
	}
}

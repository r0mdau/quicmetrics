package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
	"github.com/r0mdau/quicmetrics/internal/tlstest"
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
	var out strings.Builder
	if err := run(context.Background(), []string{"-version"}, &out); err != nil || !strings.HasPrefix(out.String(), "quicmetrics ") {
		t.Errorf("run -version = %v, %q, want nil and the version", err, out.String())
	}
	for _, args := range [][]string{
		{"-no-such-flag"},
		{"-log-level", "loud"},
		{"-tls-cert", "tls.crt"},
		{"-tls-key", "tls.key"},
		{"-tls-cert", "tls.crt", "-tls-key", "tls.key", "-tls-reload-interval", "0s"},
		{"-tls-reload-interval", "-1s"},
		{"-max-metric-names", "-1"},
		{"-max-tags", "-1"},
		{"-max-tag-key-len", "-1"},
		{"-max-tag-value-len", "-1"},
		{"-first-stream-timeout", "-1s"},
		{"-line-timeout", "-1s"},
		{"-ack-timeout", "-1s"},
		{"-series-ttl", "-1s"},
		{"-series-ttl", "soon"},
	} {
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

func TestRunWarnsAboutTheSelfSignedCertificate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0"}, &logs)
	}()
	waitForAddr(t, &logs, quicAddrRe)
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("run = %v, want nil after cancel", err)
	}
	if !regexp.MustCompile(`level=WARN msg="no -tls-cert and -tls-key: using a self-signed certificate`).MatchString(logs.String()) {
		t.Errorf("no self-signed warning in:\n%s", logs.String())
	}
}

func TestRunFailsOnInvalidCertificateFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.pem")
	if err := os.WriteFile(broken, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"missing files": {"-tls-cert", filepath.Join(dir, "missing.crt"), "-tls-key", filepath.Join(dir, "missing.key")},
		"invalid files": {"-tls-cert", broken, "-tls-key", broken},
	} {
		args = append(args, "-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0")
		err := run(context.Background(), args, io.Discard)
		if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), "create TLS config") {
			t.Errorf("%s: run = %v, want a startup error", name, err)
		}
	}
}

func TestRunServesTheConfiguredCertificateAndReloadsIt(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writePair := func(host string) {
		t.Helper()
		pair := ca.IssueServer(t, host, "127.0.0.1")
		for path, data := range map[string][]byte{certFile: pair.CertPEM, keyFile: pair.KeyPEM} {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writePair("first.test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0",
			"-tls-cert", certFile, "-tls-key", keyFile, "-tls-reload-interval", "10ms",
		}, &logs)
	}()
	quicAddr := waitForAddr(t, &logs, quicAddrRe)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	dial := func(roots *x509.CertPool) (*quic.Conn, error) {
		dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
		defer cancelDial()
		return quic.DialAddr(dialCtx, quicAddr, &tls.Config{
			RootCAs: roots, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13,
		}, nil)
	}
	peerCommonName := func() string {
		t.Helper()
		conn, err := dial(pool)
		if err != nil {
			t.Fatalf("dial with the CA: %v", err)
		}
		defer func() { _ = conn.CloseWithError(0, "") }()
		return conn.ConnectionState().TLS.PeerCertificates[0].Subject.CommonName
	}

	if got := peerCommonName(); got != "first.test" {
		t.Fatalf("served certificate = %q, want first.test", got)
	}
	if conn, err := dial(x509.NewCertPool()); err == nil {
		_ = conn.CloseWithError(0, "")
		t.Error("a client that doesn't trust the CA connected")
	}
	if strings.Contains(logs.String(), "self-signed") {
		t.Errorf("self-signed warning logged with -tls-cert:\n%s", logs.String())
	}

	writePair("second.test")
	deadline := time.Now().Add(5 * time.Second)
	for peerCommonName() != "second.test" {
		if time.Now().After(deadline) {
			t.Fatalf("certificate never reloaded; logs:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
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

func TestRunFailsOnInvalidClientCA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.pem")
	if err := os.WriteFile(broken, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"missing": filepath.Join(dir, "missing.pem"), "invalid": broken} {
		err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0", "-tls-client-ca", file}, io.Discard)
		if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), "create TLS config") {
			t.Errorf("%s: run = %v, want a startup error", name, err)
		}
	}
}

func TestRunRequiresClientCertificatesWithTLSClientCA(t *testing.T) {
	t.Parallel()
	serverCA, clientCA, otherCA := tlstest.NewCA(t, "server CA"), tlstest.NewCA(t, "client CA"), tlstest.NewCA(t, "other CA")
	dir := t.TempDir()
	files := map[string][]byte{"tls.crt": nil, "tls.key": nil, "client-ca.pem": clientCA.PEM}
	server := serverCA.IssueServer(t, "127.0.0.1")
	files["tls.crt"], files["tls.key"] = server.CertPEM, server.KeyPEM
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0",
			"-tls-cert", filepath.Join(dir, "tls.crt"), "-tls-key", filepath.Join(dir, "tls.key"),
			"-tls-client-ca", filepath.Join(dir, "client-ca.pem"),
		}, &logs)
	}()
	quicAddr := waitForAddr(t, &logs, quicAddrRe)

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(serverCA.PEM)
	exchange := func(client *tlstest.KeyPair) error {
		dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
		defer cancelDial()
		conf := &tls.Config{RootCAs: roots, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
		if client != nil {
			cert, err := tls.X509KeyPair(client.CertPEM, client.KeyPEM)
			if err != nil {
				return err
			}
			conf.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
		}
		conn, err := quic.DialAddr(dialCtx, quicAddr, conf, nil)
		if err != nil {
			return err
		}
		defer func() { _ = conn.CloseWithError(0, "") }()
		stream, err := conn.OpenStreamSync(dialCtx)
		if err != nil {
			return err
		}
		if _, err := stream.Write([]byte("mtls.hits:1|c\n")); err != nil {
			return err
		}
		ack, err := bufio.NewReader(stream).ReadString('\n')
		if err == nil && ack != "1\n" {
			err = fmt.Errorf("ack = %q, want \"1\\n\"", ack)
		}
		return err
	}

	valid, foreign := clientCA.IssueClient(t, "agent-1"), otherCA.IssueClient(t, "agent-2")
	if err := exchange(&valid); err != nil {
		t.Errorf("client with a certificate from -tls-client-ca: %v, want accepted", err)
	}
	if err := exchange(nil); err == nil {
		t.Error("client without a certificate was accepted")
	}
	if err := exchange(&foreign); err == nil {
		t.Error("client with a certificate from another CA was accepted")
	}
	if !strings.Contains(logs.String(), `client_subject="CN=agent-1"`) || strings.Contains(logs.String(), "agent-2") {
		t.Errorf("want the accepted client's subject and no other logged:\n%s", logs.String())
	}

	cancel()
	if err := <-errc; err != nil {
		t.Errorf("run = %v, want nil after cancel", err)
	}
}

func TestRunAppliesLimitFlags(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0",
			"-max-metric-names", "1", "-max-tags", "2", "-max-tag-key-len", "3", "-max-tag-value-len", "4",
		}, &logs)
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
		"limited.hits:1|c|#env:test",
		"other.hits:1|c",
		"limited.hits:1|c|#a:1,b:2,c:3",
		"limited.hits:1|c|#toolong:1",
		"limited.hits:1|c|#a:12345",
	)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + httpAddr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{
		`limited_hits_total{env="test"} 1`,
		`quicmetrics_metrics_dropped_total{reason="names"} 1`,
		`quicmetrics_metrics_dropped_total{reason="tags"} 1`,
		`quicmetrics_metrics_dropped_total{reason="tag_key_length"} 1`,
		`quicmetrics_metrics_dropped_total{reason="tag_value_length"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "other_hits") {
		t.Errorf("/metrics exposes a metric over -max-metric-names:\n%s", body)
	}

	cancel()
	if err := <-errc; err != nil {
		t.Errorf("run = %v, want nil after cancel", err)
	}
}

func TestRunClosesASessionThatOpensNoStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0", "-first-stream-timeout", "200ms"}, &logs)
	}()
	quicAddr := waitForAddr(t, &logs, quicAddrRe)

	dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDial()
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	conn, err := quic.DialAddr(dialCtx, quicAddr, tlsConf, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseWithError(0, "") }()
	select {
	case <-conn.Context().Done():
	case <-dialCtx.Done():
		t.Fatal("session was not closed")
	}
	var appErr *quic.ApplicationError
	if err := context.Cause(conn.Context()); !errors.As(err, &appErr) ||
		appErr.ErrorCode != protocol.CodeSlowClient || appErr.ErrorMessage != "no stream opened" {
		t.Errorf("close cause = %v, want application error %d with reason %q", err, protocol.CodeSlowClient, "no stream opened")
	}

	cancel()
	if err := <-errc; err != nil {
		t.Errorf("run = %v, want nil after cancel", err)
	}
}

func TestRunExpiresIdleSeries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{"-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0", "-series-ttl", "300ms"}, &logs)
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
	sendMetrics(t, stream, "fleeting.hits:3|c|#fn:a", "fleeting.level:1|g")

	scrape := func() string {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + httpAddr + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return string(body)
	}
	if body := scrape(); !strings.Contains(body, `fleeting_hits_total{fn="a"} 3`) || !strings.Contains(body, "fleeting_level 1") {
		t.Fatalf("/metrics before the TTL:\n%s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for body := scrape(); strings.Contains(body, "fleeting_"); body = scrape() {
		if time.Now().After(deadline) {
			t.Fatalf("series still exposed after the TTL:\n%s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if body := scrape(); !strings.Contains(body, "quicmetrics_series_expired_total 2") {
		t.Errorf("/metrics does not count the 2 expired series:\n%s", body)
	}

	cancel()
	if err := <-errc; err != nil {
		t.Errorf("run = %v, want nil after cancel", err)
	}
}

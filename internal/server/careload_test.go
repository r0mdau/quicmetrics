package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
	"github.com/r0mdau/quicmetrics/internal/tlstest"
)

func newTestCAReloader(t *testing.T, file string) (*CAReloader, *syncBuffer) {
	t.Helper()
	var logs syncBuffer
	r, err := NewCAReloader(file, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewCAReloader: %v", err)
	}
	return r, &logs
}

func poolTrusts(t *testing.T, r *CAReloader, pair tlstest.KeyPair) bool {
	t.Helper()
	cert, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: r.pool.Load(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err == nil
}

func TestCAReloaderSwitchesToANewBundle(t *testing.T) {
	t.Parallel()
	first, second := tlstest.NewCA(t, "first CA"), tlstest.NewCA(t, "second CA")
	file := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, file, first.PEM)
	r, logs := newTestCAReloader(t, file)
	if !poolTrusts(t, r, first.IssueClient(t, "agent")) || poolTrusts(t, r, second.IssueClient(t, "agent")) {
		t.Fatal("initial pool should trust only the first CA")
	}

	if err := r.Reload(); err != nil {
		t.Fatalf("Reload unchanged file: %v", err)
	}
	if n := strings.Count(logs.String(), "TLS client CA bundle loaded"); n != 1 {
		t.Errorf("unchanged file logged %d loads, want 1:\n%s", n, logs.String())
	}

	writeFile(t, file, second.PEM)
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload new file: %v", err)
	}
	if poolTrusts(t, r, first.IssueClient(t, "agent")) || !poolTrusts(t, r, second.IssueClient(t, "agent")) {
		t.Error("pool after reload should trust only the second CA")
	}
}

func TestCAReloaderKeepsPreviousPoolOnFailure(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	file := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, file, ca.PEM)
	r, _ := newTestCAReloader(t, file)

	for name, content := range map[string][]byte{"garbage": []byte("not a certificate"), "empty": nil} {
		writeFile(t, file, content)
		if err := r.Reload(); err == nil {
			t.Errorf("%s: Reload = nil, want an error", name)
		}
		if !poolTrusts(t, r, ca.IssueClient(t, "agent")) {
			t.Errorf("%s: previous pool was dropped", name)
		}
	}
	if err := r.Reload(); err != nil {
		t.Errorf("Reload of the same broken content = %v, want nil so Watch doesn't repeat the warning", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(); err == nil {
		t.Error("missing file: Reload = nil, want an error")
	}
	if !poolTrusts(t, r, ca.IssueClient(t, "agent")) {
		t.Error("previous pool was dropped when the file went missing")
	}
}

func TestNewCAReloaderFailsOnInvalidFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.pem")
	writeFile(t, broken, []byte("broken"))
	empty := filepath.Join(dir, "empty.pem")
	writeFile(t, empty, nil)
	for name, file := range map[string]string{"missing": filepath.Join(dir, "missing.pem"), "invalid": broken, "empty": empty} {
		if r, err := NewCAReloader(file, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s: NewCAReloader = %v, want an error", name, r)
		}
	}
}

func TestCAReloaderWatchPicksUpChangesAndWarnsOnFailure(t *testing.T) {
	t.Parallel()
	first, second := tlstest.NewCA(t, "first CA"), tlstest.NewCA(t, "second CA")
	file := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, file, first.PEM)
	r, logs := newTestCAReloader(t, file)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(ctx, 10*time.Millisecond)
	}()
	defer func() {
		cancel()
		<-done
	}()

	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(testTimeout)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; logs:\n%s", what, logs.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	writeFile(t, file, []byte("broken"))
	eventually("the warning", func() bool {
		return strings.Contains(logs.String(), "reload TLS client CA bundle failed, keeping the previous one")
	})
	writeFile(t, file, second.PEM)
	eventually("the new pool", func() bool { return poolTrusts(t, r, second.IssueClient(t, "agent")) })
}

func listenMTLS(t *testing.T, serverCA *tlstest.CA, r *CAReloader) *quic.Listener {
	t.Helper()
	pair := serverCA.IssueServer(t, "127.0.0.1")
	cert, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	conf := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	r.RequireClientCerts(conf)
	listener, err := quic.ListenAddr("127.0.0.1:0", conf, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// A TLS 1.3 client finishes its handshake before the server checks its certificate, so a
// rejection only shows on the first exchange.
func exchangeWithClientCert(ctx context.Context, listener *quic.Listener, serverCA *tlstest.CA, clientCert *tlstest.KeyPair) error {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(serverCA.PEM)
	conf := &tls.Config{RootCAs: pool, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	if clientCert != nil {
		cert, err := tls.X509KeyPair(clientCert.CertPEM, clientCert.KeyPEM)
		if err != nil {
			return err
		}
		conf.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
	}
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), conf, nil)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseWithError(0, "") }()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	return newTestClient(stream).ackOnce("a:1|c")
}

func TestRequireClientCertsAcceptsOnlyCertificatesFromTheClientCA(t *testing.T) {
	t.Parallel()
	serverCA, clientCA, otherCA := tlstest.NewCA(t, "server CA"), tlstest.NewCA(t, "client CA"), tlstest.NewCA(t, "other CA")
	file := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, file, clientCA.PEM)
	r, _ := newTestCAReloader(t, file)
	listener := listenMTLS(t, serverCA, r)
	agg, _ := newTestAggregator()
	srv, _ := newCapturingServer(agg)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	valid, wrongCA := clientCA.IssueClient(t, "agent"), otherCA.IssueClient(t, "agent")
	serverCert := serverCA.IssueServer(t, "127.0.0.1")
	for name, tc := range map[string]struct {
		cert *tlstest.KeyPair
		ok   bool
	}{
		"valid certificate":      {&valid, true},
		"no certificate":         {nil, false},
		"certificate of another": {&wrongCA, false},
		"server certificate":     {&serverCert, false},
	} {
		err := exchangeWithClientCert(ctx, listener, serverCA, tc.cert)
		if tc.ok && err != nil {
			t.Errorf("%s: %v, want the metric acknowledged", name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: the metric was acknowledged, want the client rejected", name)
		}
	}

	writeFile(t, file, otherCA.PEM)
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := exchangeWithClientCert(ctx, listener, serverCA, &wrongCA); err != nil {
		t.Errorf("certificate of the new CA after reload: %v, want accepted", err)
	}
	if err := exchangeWithClientCert(ctx, listener, serverCA, &valid); err == nil {
		t.Error("certificate of the old CA after reload was accepted, want rejected")
	}
}

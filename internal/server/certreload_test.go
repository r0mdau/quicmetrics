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

func writePair(t *testing.T, dir string, pair tlstest.KeyPair) (certFile, keyFile string) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeFile(t, certFile, pair.CertPEM)
	writeFile(t, keyFile, pair.KeyPEM)
	return certFile, keyFile
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestReloader(t *testing.T, certFile, keyFile string) (*CertReloader, *syncBuffer) {
	t.Helper()
	var logs syncBuffer
	r, err := NewCertReloader(certFile, keyFile, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	return r, &logs
}

func currentCommonName(t *testing.T, r *CertReloader) string {
	t.Helper()
	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	return cert.Leaf.Subject.CommonName
}

func TestCertReloaderSwitchesToNewFiles(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, ca.IssueServer(t, "first.test"))
	r, logs := newTestReloader(t, certFile, keyFile)
	if got := currentCommonName(t, r); got != "first.test" {
		t.Fatalf("initial certificate = %q, want first.test", got)
	}

	if err := r.Reload(); err != nil {
		t.Fatalf("Reload unchanged files: %v", err)
	}
	if n := strings.Count(logs.String(), "TLS certificate loaded"); n != 1 {
		t.Errorf("unchanged files logged %d loads, want 1:\n%s", n, logs.String())
	}

	writePair(t, dir, ca.IssueServer(t, "second.test"))
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload new files: %v", err)
	}
	if got := currentCommonName(t, r); got != "second.test" {
		t.Errorf("certificate after reload = %q, want second.test", got)
	}
	if !strings.Contains(logs.String(), "second.test") {
		t.Errorf("reload did not log the new subject:\n%s", logs.String())
	}
}

func TestCertReloaderKeepsPreviousCertificateOnFailure(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	first, second := ca.IssueServer(t, "first.test"), ca.IssueServer(t, "second.test")
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, first)
	r, _ := newTestReloader(t, certFile, keyFile)

	for name, pair := range map[string]tlstest.KeyPair{
		"garbage":           {CertPEM: []byte("not a certificate"), KeyPEM: []byte("not a key")},
		"empty":             {},
		"key from the next": {CertPEM: first.CertPEM, KeyPEM: second.KeyPEM},
	} {
		writeFile(t, certFile, pair.CertPEM)
		writeFile(t, keyFile, pair.KeyPEM)
		if err := r.Reload(); err == nil {
			t.Errorf("%s: Reload = nil, want an error", name)
		}
		if got := currentCommonName(t, r); got != "first.test" {
			t.Errorf("%s: certificate = %q, want first.test kept", name, got)
		}
	}

	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(); err == nil {
		t.Error("missing key: Reload = nil, want an error")
	}

	writePair(t, dir, second)
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload after repair: %v", err)
	}
	if got := currentCommonName(t, r); got != "second.test" {
		t.Errorf("certificate after repair = %q, want second.test", got)
	}
}

func TestCertReloaderReportsABrokenPairOnce(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, ca.IssueServer(t, "first.test"))
	r, _ := newTestReloader(t, certFile, keyFile)

	writeFile(t, certFile, []byte("broken"))
	if err := r.Reload(); err == nil {
		t.Fatal("first Reload of a broken pair = nil, want an error")
	}
	if err := r.Reload(); err != nil {
		t.Errorf("second Reload of the same broken pair = %v, want nil so Watch doesn't repeat the warning", err)
	}
}

func TestNewCertReloaderFailsOnInvalidFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	broken := filepath.Join(dir, "broken")
	writeFile(t, broken, []byte("broken"))
	empty := filepath.Join(dir, "empty")
	writeFile(t, empty, nil)
	for name, files := range map[string][2]string{
		"missing cert": {missing, broken},
		"missing key":  {broken, missing},
		"invalid":      {broken, broken},
		"empty":        {empty, empty},
	} {
		if r, err := NewCertReloader(files[0], files[1], slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s: NewCertReloader = %v, want an error", name, r)
		}
	}
}

// swapSecretVolume mimics how the kubelet updates a Secret volume: the files are symlinks
// to ..data/<name>, and ..data is atomically repointed to a new timestamped directory.
func swapSecretVolume(t *testing.T, dir, version string, pair tlstest.KeyPair) {
	t.Helper()
	versionDir := filepath.Join(dir, ".."+version)
	if err := os.Mkdir(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(versionDir, "tls.crt"), pair.CertPEM)
	writeFile(t, filepath.Join(versionDir, "tls.key"), pair.KeyPEM)
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(".."+version, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
}

func TestCertReloaderFollowsKubernetesSecretSymlinkSwap(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	dir := t.TempDir()
	swapSecretVolume(t, dir, "v1", ca.IssueServer(t, "first.test"))
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := newTestReloader(t, filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if got := currentCommonName(t, r); got != "first.test" {
		t.Fatalf("initial certificate = %q, want first.test", got)
	}

	swapSecretVolume(t, dir, "v2", ca.IssueServer(t, "second.test"))
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload after symlink swap: %v", err)
	}
	if got := currentCommonName(t, r); got != "second.test" {
		t.Errorf("certificate after symlink swap = %q, want second.test", got)
	}
}

func TestCertReloaderWatchPicksUpChangesAndWarnsOnFailure(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, ca.IssueServer(t, "first.test"))
	r, logs := newTestReloader(t, certFile, keyFile)

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

	writeFile(t, certFile, []byte("broken"))
	eventually("the warning", func() bool {
		return strings.Contains(logs.String(), "keeping the previous one")
	})
	if got := currentCommonName(t, r); got != "first.test" {
		t.Errorf("certificate after a failed reload = %q, want first.test kept", got)
	}

	writePair(t, dir, ca.IssueServer(t, "second.test"))
	eventually("the new certificate", func() bool { return currentCommonName(t, r) == "second.test" })
}

func TestCertReloaderServesNewCertificateToNewConnections(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, ca.IssueServer(t, "first.test", "127.0.0.1"))
	r, _ := newTestReloader(t, certFile, keyFile)
	listener, err := quic.ListenAddr("127.0.0.1:0", r.TLSConfig(), nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	peerCommonName := func() string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		conn, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{
			RootCAs: pool, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13,
		}, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.CloseWithError(0, "") }()
		return conn.ConnectionState().TLS.PeerCertificates[0].Subject.CommonName
	}

	if got := peerCommonName(); got != "first.test" {
		t.Fatalf("served certificate = %q, want first.test", got)
	}
	writePair(t, dir, ca.IssueServer(t, "second.test", "127.0.0.1"))
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := peerCommonName(); got != "second.test" {
		t.Errorf("served certificate after reload = %q, want second.test", got)
	}
}

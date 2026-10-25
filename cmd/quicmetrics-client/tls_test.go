package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r0mdau/quicmetrics/internal/protocol"
	"github.com/r0mdau/quicmetrics/internal/tlstest"
)

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func serverTLS(t *testing.T, pair tlstest.KeyPair) *tls.Config {
	t.Helper()
	cert, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
}

func TestTLSConfigVerifiesByDefault(t *testing.T) {
	t.Parallel()
	conf, err := tlsOptions{}.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if conf.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set without -insecure")
	}
	if conf.RootCAs != nil {
		t.Error("RootCAs is set without -tls-ca, want the system roots (nil)")
	}
	if conf.MinVersion != tls.VersionTLS13 || len(conf.NextProtos) != 1 || conf.NextProtos[0] != protocol.ALPN {
		t.Errorf("MinVersion = %#x, NextProtos = %v, want TLS 1.3 and [%s]", conf.MinVersion, conf.NextProtos, protocol.ALPN)
	}
}

func TestTLSConfigOptions(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	conf, err := tlsOptions{caFile: writeFile(t, "ca.pem", ca.PEM), serverName: "metrics.test"}.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if conf.RootCAs == nil {
		t.Error("RootCAs is nil with -tls-ca")
	}
	if conf.ServerName != "metrics.test" {
		t.Errorf("ServerName = %q, want metrics.test", conf.ServerName)
	}
	if conf.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set with -tls-ca")
	}

	insecure, err := tlsOptions{insecure: true}.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig -insecure: %v", err)
	}
	if !insecure.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is not set with -insecure")
	}
}

func TestTLSConfigRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	caFile := writeFile(t, "ca.pem", ca.PEM)
	for name, tc := range map[string]struct {
		opts      tlsOptions
		wantUsage bool
	}{
		"insecure with a CA": {tlsOptions{insecure: true, caFile: caFile}, true},
		"missing CA file":    {tlsOptions{caFile: filepath.Join(t.TempDir(), "missing.pem")}, false},
		"CA without PEM":     {tlsOptions{caFile: writeFile(t, "ca.pem", []byte("not a certificate"))}, false},
	} {
		_, err := tc.opts.tlsConfig()
		if err == nil {
			t.Errorf("%s: tlsConfig = nil, want an error", name)
		} else if errors.Is(err, errUsage) != tc.wantUsage {
			t.Errorf("%s: errors.Is(%v, errUsage) = %t, want %t", name, err, !tc.wantUsage, tc.wantUsage)
		}
	}
}

func TestSendMetricsVerifiesTheServerCertificate(t *testing.T) {
	t.Parallel()
	ca, other := tlstest.NewCA(t, "test CA"), tlstest.NewCA(t, "other CA")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	addr, _ := startTLSServer(ctx, t, serverTLS(t, ca.IssueServer(t, "metrics.test", "127.0.0.1")))
	caFile := writeFile(t, "ca.pem", ca.PEM)

	for name, tc := range map[string]struct {
		opts    tlsOptions
		wantErr string
	}{
		"trusted CA, host from the address": {opts: tlsOptions{caFile: caFile}},
		"trusted CA, explicit server name":  {opts: tlsOptions{caFile: caFile, serverName: "metrics.test"}},
		"trusted CA, wrong server name":     {opts: tlsOptions{caFile: caFile, serverName: "other.test"}, wantErr: "certificate is valid for metrics.test, not other.test"},
		"other CA":                          {opts: tlsOptions{caFile: writeFile(t, "other.pem", other.PEM)}, wantErr: "unknown authority"},
		"system roots":                      {opts: tlsOptions{}, wantErr: "unknown authority"},
		"insecure":                          {opts: tlsOptions{insecure: true}},
		"insecure, wrong server name":       {opts: tlsOptions{insecure: true, serverName: "other.test"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tlsConf, err := tc.opts.tlsConfig()
			if err != nil {
				t.Fatalf("tlsConfig: %v", err)
			}
			sent, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, tlsConf, newGenerator(1, 1), options{count: 3})
			if tc.wantErr == "" {
				if err != nil || sent != 3 {
					t.Fatalf("sendMetrics = %d, %v, want 3 metrics sent", sent, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("sendMetrics = %d, %v, want an error containing %q", sent, err, tc.wantErr)
			}
		})
	}
}

func TestRunTLSFlags(t *testing.T) {
	t.Parallel()
	ca := tlstest.NewCA(t, "test CA")
	caFile := writeFile(t, "ca.pem", ca.PEM)
	var stderr strings.Builder
	if err := run(context.Background(), []string{"-insecure", "-tls-ca", caFile}, &stderr); !errors.Is(err, errUsage) {
		t.Fatalf("run -insecure -tls-ca = %v, want errUsage", err)
	}
	if stderr.Len() == 0 {
		t.Error("run printed nothing for conflicting TLS flags")
	}
	if err := run(context.Background(), []string{"-tls-ca", filepath.Join(t.TempDir(), "missing.pem")}, io.Discard); err == nil || errors.Is(err, errUsage) {
		t.Errorf("run with a missing CA file = %v, want a plain error", err)
	}
}

func TestRunWarnsWhenInsecure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr strings.Builder
	if err := run(ctx, []string{"-insecure", "-addr", "127.0.0.1:1"}, &stderr); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if !strings.Contains(stderr.String(), "level=WARN") || !strings.Contains(stderr.String(), "-insecure") {
		t.Errorf("no -insecure warning in:\n%s", stderr.String())
	}
}

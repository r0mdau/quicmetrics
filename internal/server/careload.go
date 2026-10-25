package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// CAReloader holds the client CA pool from a PEM bundle and reloads it when the file changes.
type CAReloader struct {
	file   string
	logger *slog.Logger

	pool atomic.Pointer[x509.CertPool]

	mu  sync.Mutex
	pem []byte
}

// NewCAReloader loads the bundle in file, and fails if it is missing or holds no certificate.
func NewCAReloader(file string, logger *slog.Logger) (*CAReloader, error) {
	r := &CAReloader{file: file, logger: logger}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload switches to the pool in the file if its content changed. On error the previous pool
// stays.
func (r *CAReloader) Reload() error {
	pem, err := os.ReadFile(r.file)
	if err != nil {
		return fmt.Errorf("read TLS client CA bundle: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool.Load() != nil && bytes.Equal(pem, r.pem) {
		return nil
	}
	r.pem = pem
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("TLS client CA bundle %s contains no PEM certificate", r.file)
	}
	r.pool.Store(pool)
	r.logger.Info("TLS client CA bundle loaded", "file", r.file)
	return nil
}

// Watch calls Reload every interval until ctx is done, logging failures.
func (r *CAReloader) Watch(ctx context.Context, interval time.Duration) {
	watch(ctx, interval, r.logger, "TLS client CA bundle", r.Reload)
}

// RequireClientCerts makes conf verify client certificates against the current pool. It sets
// GetConfigForClient on conf.
func (r *CAReloader) RequireClientCerts(conf *tls.Config) {
	conf.ClientAuth = tls.RequireAndVerifyClientCert
	conf.ClientCAs = r.pool.Load()
	base := conf.Clone()
	conf.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		perConn := base.Clone()
		perConn.ClientCAs = r.pool.Load()
		return perConn, nil
	}
}

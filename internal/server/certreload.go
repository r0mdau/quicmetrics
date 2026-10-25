package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

// CertReloader serves a certificate from PEM files and reloads it when their content changes.
type CertReloader struct {
	certFile string
	keyFile  string
	logger   *slog.Logger

	current atomic.Pointer[tls.Certificate]

	mu      sync.Mutex
	certPEM []byte
	keyPEM  []byte
}

// NewCertReloader loads certFile and keyFile, and fails if they are missing or invalid.
func NewCertReloader(certFile, keyFile string, logger *slog.Logger) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile, logger: logger}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate returns the current certificate. It is a tls.Config.GetCertificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

// TLSConfig returns a TLS 1.3 server config that presents the current certificate.
func (r *CertReloader) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: r.GetCertificate,
		NextProtos:     []string{protocol.ALPN},
		MinVersion:     tls.VersionTLS13,
	}
}

// Reload switches to the certificate in the files if their content changed. On error the
// previous certificate stays.
func (r *CertReloader) Reload() error {
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return fmt.Errorf("read TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return fmt.Errorf("read TLS private key: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current.Load() != nil && bytes.Equal(certPEM, r.certPEM) && bytes.Equal(keyPEM, r.keyPEM) {
		return nil
	}
	r.certPEM, r.keyPEM = certPEM, keyPEM
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load TLS certificate %s and key %s: %w", r.certFile, r.keyFile, err)
	}
	r.current.Store(&cert)
	r.logger.Info("TLS certificate loaded", "cert", r.certFile,
		"subject", cert.Leaf.Subject.String(), "not_after", cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// Watch calls Reload every interval until ctx is done, logging failures.
func (r *CertReloader) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reload(); err != nil {
				r.logger.Warn("reload TLS certificate failed, keeping the previous one", "err", err)
			}
		}
	}
}

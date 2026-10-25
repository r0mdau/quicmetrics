package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

// tlsOptions holds the TLS flags of the client.
type tlsOptions struct {
	caFile     string
	certFile   string
	keyFile    string
	serverName string
	insecure   bool
}

// tlsConfig builds the client TLS config. Only insecure skips verification.
func (o tlsOptions) tlsConfig() (*tls.Config, error) {
	if o.insecure && o.caFile != "" {
		return nil, fmt.Errorf("%w: -insecure and -tls-ca are mutually exclusive", errUsage)
	}
	if (o.certFile == "") != (o.keyFile == "") {
		return nil, fmt.Errorf("%w: -tls-cert and -tls-key must be set together", errUsage)
	}
	conf := &tls.Config{
		ServerName: o.serverName,
		NextProtos: []string{protocol.ALPN},
		MinVersion: tls.VersionTLS13,
	}
	if o.certFile != "" {
		cert, err := tls.LoadX509KeyPair(o.certFile, o.keyFile)
		if err != nil {
			return nil, fmt.Errorf("load -tls-cert and -tls-key: %w", err)
		}
		conf.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
	}
	if o.insecure {
		conf.InsecureSkipVerify = true //nolint:gosec // explicit opt-in with -insecure
		return conf, nil
	}
	if o.caFile != "" {
		pem, err := os.ReadFile(o.caFile)
		if err != nil {
			return nil, fmt.Errorf("read -tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-tls-ca %s contains no PEM certificate", o.caFile)
		}
		conf.RootCAs = pool
	}
	return conf, nil
}

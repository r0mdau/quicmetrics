package server

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"slices"
	"testing"
	"time"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

func TestSelfSignedTLSConfig(t *testing.T) {
	t.Parallel()
	conf, err := SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("SelfSignedTLSConfig: %v", err)
	}
	if !slices.Equal(conf.NextProtos, []string{protocol.ALPN}) {
		t.Errorf("NextProtos = %v, want [%s]", conf.NextProtos, protocol.ALPN)
	}
	if conf.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", conf.MinVersion)
	}
	if len(conf.Certificates) != 1 || len(conf.Certificates[0].Certificate) != 1 {
		t.Fatalf("want exactly one certificate, got %+v", conf.Certificates)
	}
	cert, err := x509.ParseCertificate(conf.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("public key is %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := cert.VerifyHostname(host); err != nil {
			t.Errorf("VerifyHostname(%q): %v", host, err)
		}
	}
	if now := time.Now(); now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		t.Errorf("certificate not valid now: %v to %v", cert.NotBefore, cert.NotAfter)
	}
}

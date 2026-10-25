// Package tlstest issues throwaway certificates for tests. Import it from _test.go files only.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// CA is a self-signed certificate authority.
type CA struct {
	PEM []byte

	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// KeyPair is a PEM certificate and its PKCS #8 private key.
type KeyPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA returns a new CA with the given common name.
func NewCA(t testing.TB, commonName string) *CA {
	t.Helper()
	key := newKey(t)
	template := &x509.Certificate{
		SerialNumber:          newSerial(t),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return &CA{PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert: cert, key: key}
}

// IssueServer returns a server certificate for hosts (DNS names or IPs).
func (ca *CA) IssueServer(t testing.TB, hosts ...string) KeyPair {
	t.Helper()
	if len(hosts) == 0 {
		t.Fatal("IssueServer needs at least one host")
	}
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0]},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}
	return ca.issue(t, template)
}

// IssueClient returns a client certificate.
func (ca *CA) IssueClient(t testing.TB, commonName string) KeyPair {
	t.Helper()
	return ca.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

func (ca *CA) issue(t testing.TB, template *x509.Certificate) KeyPair {
	t.Helper()
	key := newKey(t)
	template.SerialNumber = newSerial(t)
	template.NotBefore = time.Now().Add(-time.Hour)
	template.NotAfter = time.Now().Add(24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return KeyPair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func newSerial(t testing.TB) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatalf("generate serial number: %v", err)
	}
	return serial
}

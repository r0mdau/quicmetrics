package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"strings"

	"github.com/lucas-clemente/quic-go"
	"github.com/narqo/go-dogstatsd-parser"
)

const addr = "localhost:4242"

type QuicMetrics map[string]*dogstatsd.Metric

var qm = QuicMetrics{}

// We start a server echoing data on the first stream the client opens,
// then connect with a client, send the message, and wait for its receipt.
func main() {
	listener, err := quic.ListenAddr(addr, generateTLSConfig(), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("session accepted: %s", conn.RemoteAddr().String())

		go func() {
			defer func() {
				_ = conn.CloseWithError(0, "bye")
				log.Printf("close session: %s", conn.RemoteAddr().String())
			}()
			stream, err := conn.AcceptStream(context.Background())
			if err != nil {
				log.Fatal(err)
			}
			// Echo through the loggingWriter
			_, err = io.Copy(loggingWriter{stream}, stream)
			if err != nil {
				log.Fatal(err)
			}
		}()
	}
}

// A wrapper for io.Writer
type loggingWriter struct{ io.Writer }

func (w loggingWriter) Write(b []byte) (int, error) {
	raw := string(b)
	fmt.Printf("Server: Got '%s'\n", raw)

	m, err := dogstatsd.Parse(raw)
	if err != nil {
		log.Fatal(err)
	}

	mKey := uniqueMetricKey(m.Name, raw)
	if _, ok := qm[mKey]; !ok {
		qm[mKey] = m
	} else {
		if m.Type == dogstatsd.Counter {
			qm[mKey].Value = int64(qm[mKey].Value.(int64) + m.Value.(int64))
		}
	}
	outputQuicMetrics(mKey, qm)

	return w.Writer.Write(b)
}

func uniqueMetricKey(name, raw string) string {
	splitTags := strings.SplitN(raw, "#", 2)
	return fmt.Sprintf("%s%s", name, splitTags[1])
}

func outputQuicMetrics(mKey string, qm QuicMetrics) {
	m := qm[mKey]
	fmt.Println(mKey, m.Value)
	for k, v := range m.Tags {
		fmt.Println(k + " - " + v)
	}
}

// Setup a bare-bones TLS config for the server
func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"quic-echo-example"},
	}
}

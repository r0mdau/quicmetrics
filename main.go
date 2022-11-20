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
	"net/http"

	"github.com/lucas-clemente/quic-go"
	"github.com/narqo/go-dogstatsd-parser"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/instrument"
	smetric "go.opentelemetry.io/otel/sdk/metric"
)

const addr = "localhost:4242"

var meter metric.Meter

func main() {
	// Start the prometheus HTTP server and pass the exporter Collector to it
	go serveMetrics()

	log.Println("Serving QUIC metrics listener at:", addr)
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
				fmt.Println(err)
			}
			// Aggregate through the loggingWriter
			_, err = io.Copy(loggingWriter{stream}, stream)
			if err != nil {
				fmt.Println(err)
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
		fmt.Println(err)
	}

	if m.Type == dogstatsd.Counter {
		counter, err := meter.SyncFloat64().Counter(m.Name, instrument.WithDescription("a simple counter"))
		if err != nil {
			log.Fatal(err)
		}
		attrs := []attribute.KeyValue{}
		for k, v := range m.Tags {
			attrs = append(attrs, attribute.Key(k).String(v))
		}
		counter.Add(context.Background(), float64(m.Value.(int64)), attrs...)
	}

	return w.Writer.Write(b)
}

func serveMetrics() {
	exporter, err := prometheus.New()
	if err != nil {
		log.Fatal(err)
	}
	provider := smetric.NewMeterProvider(smetric.WithReader(exporter))
	meter = provider.Meter("github.com/open-telemetry/opentelemetry-go/example/prometheus")

	log.Println("Serving metrics at: localhost:2223/metrics")

	http.Handle("/metrics", promhttp.Handler())
	err = http.ListenAndServe(":2223", nil)
	if err != nil {
		fmt.Printf("error serving http: %v", err)
		return
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

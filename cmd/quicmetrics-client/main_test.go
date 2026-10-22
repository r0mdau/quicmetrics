package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/r0mdau/quicmetrics/internal/server"
)

func TestRunSendsSampleMetricsToServer(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	srv := server.New(server.NewAggregator(meter, logger), logger)

	tlsConf, err := server.SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConf, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, listener) }()

	if err := run(ctx, logger, listener.Addr().String(), sampleMetrics); err != nil {
		t.Fatalf("run: %v", err)
	}
	_ = listener.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[float64])
			if !ok {
				t.Fatalf("metric %q: unexpected aggregation %T", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				got[m.Name+"{"+dp.Attributes.Encoded(attribute.DefaultEncoder())+"}"] = dp.Value
			}
		}
	}
	want := map[string]float64{
		"users.online{city=beijing,country=china}":  1,
		"users.online{city=losangeles,country=usa}": 2,
		"users.online{country=usa}":                 3,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestRunFailsWithoutServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := run(ctx, slog.New(slog.DiscardHandler), "127.0.0.1:1", sampleMetrics); err == nil {
		t.Fatal("run returned nil without a server")
	}
}

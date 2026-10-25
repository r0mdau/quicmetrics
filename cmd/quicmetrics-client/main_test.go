package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/r0mdau/quicmetrics/internal/server"
)

func TestSendMetricsDeliversGeneratedMetrics(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, reader := startServer(ctx, t)

	const seed, functions, count = 99, 3, 200
	sent, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(seed, functions), options{count: count})
	if err != nil {
		t.Fatalf("sendMetrics: %v", err)
	}
	if sent != count {
		t.Fatalf("sent %d metrics, want %d", sent, count)
	}

	wantReader := sdkmetric.NewManualReader()
	wantAgg := server.NewAggregator(sdkmetric.NewMeterProvider(sdkmetric.WithReader(wantReader)).Meter("test"), slog.New(slog.DiscardHandler))
	gen := newGenerator(seed, functions)
	for range count {
		wantAgg.Record(ctx, gen.next())
	}
	got, want := collectValues(t, reader), collectValues(t, wantReader)
	if !maps.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for _, name := range []string{"faas.concurrency", "faas.memory.utilization", "faas.duration", "http.latency"} {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(got)), func(series string) bool { return strings.HasPrefix(series, name+"{") }) {
			t.Errorf("server recorded no %s series", name)
		}
	}
}

func TestSendMetricsDeliversBatches(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		opts options
	}{
		{"unpaced", options{count: 1000, batch: 50}},
		{"unpaced, count not a multiple of the batch", options{count: 1001, batch: 64}},
		{"unpaced, batch larger than count", options{count: 7, batch: 100}},
		{"paced", options{count: 600, batch: 25, interval: 100 * time.Microsecond}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			addr, reader := startServer(ctx, t)

			const seed, functions = 5, 4
			acked, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(seed, functions), tt.opts)
			if err != nil {
				t.Fatalf("sendMetrics: %v", err)
			}
			if acked != tt.opts.count {
				t.Fatalf("acknowledged %d metrics, want %d", acked, tt.opts.count)
			}

			wantReader := sdkmetric.NewManualReader()
			wantAgg := server.NewAggregator(sdkmetric.NewMeterProvider(sdkmetric.WithReader(wantReader)).Meter("test"), slog.New(slog.DiscardHandler))
			gen := newGenerator(seed, functions)
			for range tt.opts.count {
				wantAgg.Record(ctx, gen.next())
			}
			if got, want := collectValues(t, reader), collectValues(t, wantReader); !maps.Equal(got, want) {
				t.Fatalf("got %v\nwant %v", got, want)
			}
		})
	}
}

func TestSendMetricsLogsEverySentMetricAtDebug(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, _ := startServer(ctx, t)

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := sendMetrics(ctx, logger, addr, insecureTLS(t), newGenerator(1, 1), options{count: 30, batch: 10}); err != nil {
		t.Fatalf("sendMetrics: %v", err)
	}
	if got := strings.Count(logs.String(), `msg="metric sent"`); got != 30 {
		t.Errorf("logged %d sent metrics, want 30", got)
	}
	if !strings.Contains(logs.String(), "count=30") {
		t.Errorf("missing the final count in the logs:\n%s", logs.String())
	}
}

func TestSendMetricsRejectsInvalidAcks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		ack  string
	}{
		{"more than sent", "5000\n"},
		{"zero", "0\n"},
		{"negative", "-1\n"},
		{"not a number", "ok\n"},
		{"echoed metric", "foo:1|c\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			addr := startAckServer(ctx, t, tt.ack)

			_, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(1, 1), options{count: 3, batch: 3})
			if err == nil {
				t.Fatal("sendMetrics accepted an invalid ack")
			}
		})
	}
}

func TestSendMetricsFailsWhenServerCloses(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := startAckServer(ctx, t, "")

	if _, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(1, 1), options{count: 3, batch: 3}); err == nil {
		t.Fatal("sendMetrics returned nil although the server closed the session without acks")
	}
}

func TestSendMetricsStopsAfterDuration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, _ := startServer(ctx, t)

	start := time.Now()
	sent, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(1, 1), options{interval: 10 * time.Millisecond, duration: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("sendMetrics: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("sendMetrics took %v, want about 300ms", elapsed)
	}
	if sent > 31 {
		t.Fatalf("sent %d metrics, want at most 31 at 100/s for 300ms", sent)
	}
}

func TestSendMetricsSendsFirstMetricWithoutWaiting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, _ := startServer(ctx, t)

	sent, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(1, 1), options{interval: 10 * time.Second, duration: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("sendMetrics: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent %d metrics, want 1", sent)
	}
}

func TestSendMetricsUnpacedStopsOnCancel(t *testing.T) {
	t.Parallel()
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelServer()
	addr, reader := startServer(serverCtx, t)

	ctx, cancel := context.WithCancel(serverCtx)
	defer cancel()
	go func() {
		for ctx.Err() == nil && !hasRecorded(ctx, reader) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	if _, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), addr, insecureTLS(t), newGenerator(1, 1), options{}); err != nil {
		t.Fatalf("sendMetrics: %v", err)
	}
	if serverCtx.Err() != nil {
		t.Fatalf("sendMetrics ran until the server context expired, %v after starting", time.Since(start))
	}
	if !hasRecorded(context.Background(), reader) {
		t.Fatal("server recorded no metrics before cancel")
	}
}

func TestSendMetricsFailsWithoutServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := sendMetrics(ctx, slog.New(slog.DiscardHandler), "127.0.0.1:1", insecureTLS(t), newGenerator(1, 1), options{count: 1}); err == nil {
		t.Fatal("sendMetrics returned nil without a server")
	}
}

func TestRunRejectsInvalidFlags(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-rate", "-1"},
		{"-rate", "NaN"},
		{"-rate", "Inf"},
		{"-rate", "2e9"},
		{"-rate", "1e-12"},
		{"-count", "-1"},
		{"-batch", "0"},
		{"-batch", "10001"},
		{"-duration", "-1s"},
		{"-functions", "0"},
		{"-functions", "11"},
		{"-unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var stderr strings.Builder
			if err := run(context.Background(), args, &stderr); !errors.Is(err, errUsage) {
				t.Fatalf("run(%v) = %v, want errUsage", args, err)
			}
			if stderr.Len() == 0 {
				t.Fatalf("run(%v) printed nothing", args)
			}
		})
	}
}

func TestRunHelpIsNotAnError(t *testing.T) {
	t.Parallel()
	if err := run(context.Background(), []string{"-h"}, io.Discard); err != nil {
		t.Fatalf("run(-h) = %v, want nil", err)
	}
}

func TestRunCanceledWhileConnectingIsNotAnError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, []string{"-addr", "127.0.0.1:1"}, io.Discard); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}

func startAckServer(ctx context.Context, t *testing.T, ack string) (addr string) {
	t.Helper()
	tlsConf, err := server.SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConf, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		_, _ = stream.Read(make([]byte, 4096))
		if ack != "" {
			_, _ = stream.Write([]byte(ack))
			time.Sleep(100 * time.Millisecond)
		}
		_ = conn.CloseWithError(0, "bye")
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return listener.Addr().String()
}

func insecureTLS(t *testing.T) *tls.Config {
	t.Helper()
	conf, err := tlsOptions{insecure: true}.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	return conf
}

func startServer(ctx context.Context, t *testing.T) (addr string, reader *sdkmetric.ManualReader) {
	t.Helper()
	tlsConf, err := server.SelfSignedTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	return startTLSServer(ctx, t, tlsConf)
}

func startTLSServer(ctx context.Context, t *testing.T, tlsConf *tls.Config) (addr string, reader *sdkmetric.ManualReader) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	reader = sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	srv := server.New(server.NewAggregator(meter, logger), logger)

	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConf, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return listener.Addr().String(), reader
}

func hasRecorded(ctx context.Context, reader *sdkmetric.ManualReader) bool {
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		return false
	}
	for _, sm := range rm.ScopeMetrics {
		if len(sm.Metrics) > 0 {
			return true
		}
	}
	return false
}

// collectValues flattens the collected metrics into one number per series. A histogram gives
// two entries, its count and its sum, suffixed with ".count" and ".sum".
func collectValues(t *testing.T, reader *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	values := map[string]float64{}
	series := func(name string, attrs attribute.Set) string {
		return name + "{" + attrs.Encoded(attribute.DefaultEncoder()) + "}"
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, dp := range data.DataPoints {
					values[series(m.Name, dp.Attributes)] = dp.Value
				}
			case metricdata.Gauge[float64]:
				for _, dp := range data.DataPoints {
					values[series(m.Name, dp.Attributes)] = dp.Value
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					key := series(m.Name, dp.Attributes)
					values[key+".count"] = float64(dp.Count)
					values[key+".sum"] = dp.Sum
				}
			default:
				t.Fatalf("metric %q: unexpected aggregation %T", m.Name, m.Data)
			}
		}
	}
	return values
}

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	smetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func useReader(t *testing.T, reader smetric.Reader) {
	t.Helper()
	previous := meter
	meter = smetric.NewMeterProvider(smetric.WithReader(reader)).Meter("test")
	t.Cleanup(func() { meter = previous })
}

func newTestReader(t *testing.T) smetric.Reader {
	t.Helper()
	reader := smetric.NewManualReader()
	useReader(t, reader)
	return reader
}

func collectSums(t *testing.T, reader smetric.Reader) map[string]metricdata.Sum[float64] {
	t.Helper()
	rm, err := reader.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	sums := map[string]metricdata.Sum[float64]{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[float64])
			if !ok {
				t.Fatalf("metric %q: unexpected aggregation %T", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				key := m.Name + "{" + dp.Attributes.Encoded(attribute.DefaultEncoder()) + "}"
				sums[key] = metricdata.Sum[float64]{
					DataPoints:  []metricdata.DataPoint[float64]{dp},
					Temporality: sum.Temporality,
					IsMonotonic: sum.IsMonotonic,
				}
			}
		}
	}
	return sums
}

func values(sums map[string]metricdata.Sum[float64]) map[string]float64 {
	out := make(map[string]float64, len(sums))
	for k, s := range sums {
		out[k] = s.DataPoints[0].Value
	}
	return out
}

func write(t *testing.T, w io.Writer, msg string) {
	t.Helper()
	n, err := w.Write([]byte(msg))
	if err != nil {
		t.Fatalf("Write(%q): %v", msg, err)
	}
	if n != len(msg) {
		t.Fatalf("Write(%q) = %d bytes, want %d", msg, n, len(msg))
	}
}

func assertValues(t *testing.T, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got series %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("series %s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

func TestWriteEchoesInput(t *testing.T) {
	newTestReader(t)

	for _, msg := range []string{
		"users.online:1|c|#country:france",
		"temperature:21.5|g",
		"not a metric",
		"",
	} {
		t.Run(msg, func(t *testing.T) {
			var buf bytes.Buffer
			write(t, loggingWriter{&buf}, msg)
			if buf.String() != msg {
				t.Errorf("echo = %q, want %q", buf.String(), msg)
			}
		})
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWritePropagatesEchoError(t *testing.T) {
	newTestReader(t)
	want := errors.New("stream closed")

	_, err := loggingWriter{failingWriter{want}}.Write([]byte("foo:1|c"))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestCounterAggregation(t *testing.T) {
	tests := []struct {
		name     string
		messages []string
		want     map[string]float64
	}{
		{
			name:     "single counter without tags",
			messages: []string{"foo:1|c"},
			want:     map[string]float64{"foo{}": 1},
		},
		{
			name:     "tags become attributes",
			messages: []string{"foo:2|c|#a:x,b:y"},
			want:     map[string]float64{"foo{a=x,b=y}": 2},
		},
		{
			name:     "same series accumulates",
			messages: []string{"foo:1|c|#a:x", "foo:2|c|#a:x", "foo:3|c|#a:x"},
			want:     map[string]float64{"foo{a=x}": 6},
		},
		{
			name: "different tag sets are separate series",
			messages: []string{
				"users.online:1|c|#country:china,city:beijing",
				"users.online:2|c|#country:usa,city:losangeles",
				"users.online:3|c|#country:usa",
			},
			want: map[string]float64{
				"users.online{city=beijing,country=china}":  1,
				"users.online{city=losangeles,country=usa}": 2,
				"users.online{country=usa}":                 3,
			},
		},
		{
			name:     "different names are separate metrics",
			messages: []string{"foo:1|c", "bar:5|c"},
			want:     map[string]float64{"foo{}": 1, "bar{}": 5},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := newTestReader(t)
			w := loggingWriter{io.Discard}
			for _, msg := range tt.messages {
				write(t, w, msg)
			}
			assertValues(t, values(collectSums(t, reader)), tt.want)
		})
	}
}

func TestCounterIsMonotonicAndCumulative(t *testing.T) {
	reader := newTestReader(t)
	write(t, loggingWriter{io.Discard}, "foo:1|c")

	sum, ok := collectSums(t, reader)["foo{}"]
	if !ok {
		t.Fatal("foo was not recorded")
	}
	if !sum.IsMonotonic {
		t.Error("counter should be monotonic")
	}
	if sum.Temporality != metricdata.CumulativeTemporality {
		t.Errorf("temporality = %v, want cumulative", sum.Temporality)
	}
}

func TestIgnoredInputRecordsNothing(t *testing.T) {
	for _, msg := range []string{
		"temperature:21.5|g",
		"latency:12|h",
		"request.time:320|ms",
		"users.unique:alice|s",
		"garbage",
		"foo:1",
		"foo:abc|c",
		"foo:1|x",
		"",
	} {
		t.Run(msg, func(t *testing.T) {
			reader := newTestReader(t)
			write(t, loggingWriter{io.Discard}, msg)
			if got := collectSums(t, reader); len(got) != 0 {
				t.Errorf("recorded %v, want nothing", values(got))
			}
		})
	}
}

func TestPrometheusExposition(t *testing.T) {
	registry := promclient.NewRegistry()
	exporter, err := prometheus.New(prometheus.WithRegisterer(registry))
	if err != nil {
		t.Fatalf("exporter: %v", err)
	}
	useReader(t, exporter)

	w := loggingWriter{io.Discard}
	write(t, w, "users.online:1|c|#country:usa")
	write(t, w, "users.online:2|c|#country:usa")

	srv := httptest.NewServer(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	for _, want := range []string{
		"# TYPE users_online_total counter",
		`users_online_total{country="usa"} 3`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics missing %q:\n%s", want, body)
		}
	}
}

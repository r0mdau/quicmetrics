package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/narqo/go-dogstatsd-parser"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var discardLogger = slog.New(slog.DiscardHandler)

func newAggregator(reader sdkmetric.Reader) *Aggregator {
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	return NewAggregator(meter, discardLogger)
}

func newTestAggregator() (*Aggregator, sdkmetric.Reader) {
	reader := sdkmetric.NewManualReader()
	return newAggregator(reader), reader
}

func collectSums(t *testing.T, reader sdkmetric.Reader) map[string]metricdata.Sum[float64] {
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

func scrape(t *testing.T, registry *promclient.Registry) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	return string(body)
}

func newPrometheusAggregator(t *testing.T) (*Aggregator, *promclient.Registry) {
	t.Helper()
	registry := promclient.NewRegistry()
	exporter, err := prometheus.New(prometheus.WithRegisterer(registry))
	if err != nil {
		t.Fatalf("exporter: %v", err)
	}
	return newAggregator(exporter), registry
}

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "counter", raw: "foo:1|c|#a:b"},
		{name: "gauge", raw: "temperature:21.5|g"},
		{name: "empty trailing segment panics in the parser", raw: "foo:1|c|", wantErr: true},
		{name: "empty middle segment panics in the parser", raw: "foo:1|c||#a:b", wantErr: true},
		{name: "invalid UTF-8 name", raw: "foo\xff:1|c", wantErr: true},
		{name: "invalid UTF-8 tag", raw: "foo:1|c|#k:v\xff", wantErr: true},
		{name: "garbage", raw: "garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, err := Parse(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want an error", tt.raw, m)
				}
				if m != nil {
					t.Errorf("Parse(%q) metric = %+v, want nil", tt.raw, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.raw, err)
			}
		})
	}
}

func TestCounterAggregation(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			agg, reader := newTestAggregator()
			for _, msg := range tt.messages {
				agg.Record(context.Background(), msg)
			}
			assertValues(t, values(collectSums(t, reader)), tt.want)
		})
	}
}

func TestCounterIsMonotonicAndCumulative(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	agg.Record(context.Background(), "foo:1|c")

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
	t.Parallel()
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
			t.Parallel()
			agg, reader := newTestAggregator()
			agg.Record(context.Background(), msg)
			if got := collectSums(t, reader); len(got) != 0 {
				t.Errorf("recorded %v, want nothing", values(got))
			}
		})
	}
}

func TestRecordCounterValueTypes(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	ctx := context.Background()
	agg.recordCounter(ctx, &dogstatsd.Metric{Type: dogstatsd.Counter, Name: "ints", Value: int64(2)})
	agg.recordCounter(ctx, &dogstatsd.Metric{Type: dogstatsd.Counter, Name: "floats", Value: 1.5})
	agg.recordCounter(ctx, &dogstatsd.Metric{Type: dogstatsd.Counter, Name: "strings", Value: "x"})
	agg.recordCounter(ctx, &dogstatsd.Metric{Type: dogstatsd.Counter, Name: "nils"})

	assertValues(t, values(collectSums(t, reader)), map[string]float64{
		"ints{}":   2,
		"floats{}": 1.5,
	})
}

func TestPrometheusExposition(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	agg.Record(context.Background(), "users.online:1|c|#country:usa")
	agg.Record(context.Background(), "users.online:2|c|#country:usa")

	body := scrape(t, registry)
	for _, want := range []string{
		"# TYPE users_online_total counter",
		`users_online_total{country="usa"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q:\n%s", want, body)
		}
	}
}

func TestInvalidUTF8IsNotRecordedAndScrapeSurvives(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	for _, msg := range []string{"foo\xff:1|c", "a\xff:1|c", "ok:1|c|#k:v\xff", "ok:1|c|#k\xff:v", "ok:1|c"} {
		agg.Record(context.Background(), msg)
	}

	if body := scrape(t, registry); !strings.Contains(body, "ok_total 1") {
		t.Errorf("/metrics missing ok_total 1:\n%s", body)
	}
}

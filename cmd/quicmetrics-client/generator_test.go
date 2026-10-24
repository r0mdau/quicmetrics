package main

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/narqo/go-dogstatsd-parser"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/r0mdau/quicmetrics/internal/server"
)

func TestGeneratorIsDeterministicForASeed(t *testing.T) {
	t.Parallel()
	a := newGenerator(42, len(functionNames))
	b := newGenerator(42, len(functionNames))
	c := newGenerator(43, len(functionNames))
	same, different := true, false
	for range 100 {
		ma, mb, mc := a.next(), b.next(), c.next()
		same = same && ma == mb
		different = different || ma != mc
	}
	if !same {
		t.Error("two generators with the same seed produced different metrics")
	}
	if !different {
		t.Error("generators with different seeds produced the same metrics")
	}
}

func TestGeneratorEmitsCountersGaugesAndHistogramsInRange(t *testing.T) {
	t.Parallel()
	gen := newGenerator(5, len(functionNames))
	seen, beyond := map[string]int{}, map[string]int{}
	const lines = 10000
	for range lines {
		raw := gen.next()
		m, err := server.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		seen[m.Name]++
		switch m.Name {
		case "faas.concurrency":
			v, ok := m.Value.(float64)
			if m.Type != dogstatsd.Gauge || !ok || v != math.Trunc(v) || v < 0 || v > maxConcurrency {
				t.Fatalf("%q: want a gauge with an integer value in [0, %d]", raw, maxConcurrency)
			}
		case "faas.memory.utilization":
			v, ok := m.Value.(float64)
			if m.Type != dogstatsd.Gauge || !ok || v < 0 || v > 1 {
				t.Fatalf("%q: want a gauge with a value in [0, 1]", raw)
			}
		case "faas.invocations", "http.requests", "faas.billed.ms", "faas.errors":
			if v, ok := m.Value.(int64); m.Type != dogstatsd.Counter || !ok || v <= 0 {
				t.Fatalf("%q: want a counter with a positive integer value", raw)
			}
		case "faas.duration", "http.latency":
			wantType := dogstatsd.Histogram
			if m.Name == "http.latency" {
				wantType = dogstatsd.Timer
			}
			v, ok := m.Value.(float64)
			if m.Type != wantType || !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				t.Fatalf("%q: want a %q with a finite non-negative value", raw, wantType)
			}
			if v > 10000 {
				beyond[m.Name]++
			}
			if len(m.Tags) != map[string]int{"faas.duration": 2, "http.latency": 3}[m.Name] {
				t.Fatalf("%q: unexpected tags %v", raw, m.Tags)
			}
		default:
			t.Fatalf("%q: unexpected metric %q", raw, m.Name)
		}
	}
	if len(seen) != 8 {
		t.Errorf("generated %v, want all four counters, both gauges and both histograms", seen)
	}
	gauges := seen["faas.concurrency"] + seen["faas.memory.utilization"]
	if gauges < lines*10/100 || gauges > lines*16/100 {
		t.Errorf("%d of %d lines are gauges, want about 13%%", gauges, lines)
	}
	histograms := seen["faas.duration"] + seen["http.latency"]
	if histograms < lines*12/100 || histograms > lines*18/100 {
		t.Errorf("%d of %d lines are histograms, want about 15%%", histograms, lines)
	}
	for name, n := range beyond {
		if n > seen[name]/100 {
			t.Errorf("%d of %d %s values exceed the last default bucket (10000), want about none", n, seen[name], name)
		}
	}
}

func TestGeneratorHistogramsSpreadAcrossBuckets(t *testing.T) {
	t.Parallel()
	gen := newGenerator(11, len(functionNames))
	bounds := []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}
	filled := map[string]map[int]bool{"faas.duration": {}, "http.latency": {}}
	for range 20000 {
		m, err := server.Parse(gen.next())
		if err != nil {
			t.Fatal(err)
		}
		if buckets, ok := filled[m.Name]; ok {
			buckets[sort.SearchFloat64s(bounds, m.Value.(float64))] = true
		}
	}
	for name, buckets := range filled {
		if len(buckets) < 8 {
			t.Errorf("%s fills %d buckets, want values spread over at least 8", name, len(buckets))
		}
	}
}

func TestGeneratorMetricsAreRecordedAndExposedWithoutErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var logs strings.Builder
	registry := prometheus.NewRegistry()
	provider, err := server.NewPrometheusMeterProvider(registry)
	if err != nil {
		t.Fatalf("meter provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })
	agg := server.NewAggregator(provider.Meter("test"), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	gen := newGenerator(7, len(functionNames))
	for range 10000 {
		raw := gen.next()
		m, err := server.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if !slices.Contains([]dogstatsd.MetricType{dogstatsd.Counter, dogstatsd.Gauge, dogstatsd.Histogram, dogstatsd.Timer}, m.Type) {
			t.Fatalf("%q: type %q, want counter, gauge or histogram", raw, m.Type)
		}
		agg.Record(ctx, raw)
	}
	if logs.Len() > 0 {
		t.Fatalf("aggregator warnings:\n%s", logs.String())
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	types := map[string]string{}
	for _, f := range families {
		names[f.GetName()] = true
		types[f.GetName()] = f.GetType().String()
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "otel_metric_overflow" {
					t.Fatalf("%s overflowed the cardinality limit", f.GetName())
				}
			}
		}
	}
	for _, bad := range []string{"faas_concurrency_total", "faas_memory_utilization_total"} {
		if names[bad] {
			t.Errorf("gauge exposed as %s, want no _total suffix", bad)
		}
	}
	for _, want := range []string{
		"faas_invocations_total", "http_requests_total", "faas_billed_ms_total", "faas_errors_total",
		"faas_concurrency", "faas_memory_utilization",
	} {
		if !names[want] {
			t.Errorf("missing series %s", want)
		}
	}
	for _, want := range []string{"faas_duration", "http_latency"} {
		if types[want] != "HISTOGRAM" {
			t.Errorf("family %s has type %q, want HISTOGRAM", want, types[want])
		}
		if names[want+"_total"] {
			t.Errorf("histogram exposed as %s_total, want no _total suffix", want)
		}
	}
}

func TestGeneratorUsesOnlyRequestedFunctions(t *testing.T) {
	t.Parallel()
	gen := newGenerator(3, 2)
	for range 1000 {
		raw := gen.next()
		m, err := server.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if fn := m.Tags["function"]; fn != "checkout" && fn != "auth" {
			t.Fatalf("%q uses function %q, want one of the first two", raw, fn)
		}
	}
}

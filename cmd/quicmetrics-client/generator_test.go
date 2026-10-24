package main

import (
	"context"
	"log/slog"
	"math"
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

func TestGeneratorEmitsCountersAndGaugesInRange(t *testing.T) {
	t.Parallel()
	gen := newGenerator(5, len(functionNames))
	seen := map[string]int{}
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
		default:
			t.Fatalf("%q: unexpected metric %q", raw, m.Name)
		}
	}
	if len(seen) != 6 {
		t.Errorf("generated %v, want all four counters and both gauges", seen)
	}
	gauges := seen["faas.concurrency"] + seen["faas.memory.utilization"]
	if gauges < lines*10/100 || gauges > lines*20/100 {
		t.Errorf("%d of %d lines are gauges, want about 15%%", gauges, lines)
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
		if m.Type != dogstatsd.Counter && m.Type != dogstatsd.Gauge {
			t.Fatalf("%q: type %q, want counter or gauge", raw, m.Type)
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
	for _, f := range families {
		names[f.GetName()] = true
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

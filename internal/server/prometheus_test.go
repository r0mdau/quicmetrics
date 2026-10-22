package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func newPrometheusAggregator(t *testing.T) (*Aggregator, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	provider, err := NewPrometheusMeterProvider(registry)
	if err != nil {
		t.Fatalf("NewPrometheusMeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return NewAggregator(provider.Meter("test"), discardLogger), registry
}

func scrape(t *testing.T, registry *prometheus.Registry) string {
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

func assertContains(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics missing %q:\n%s", w, body)
		}
	}
}

func TestPrometheusExposition(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	agg.Record(context.Background(), "users.online:1|c|#country:usa")
	agg.Record(context.Background(), "users.online:2|c|#country:usa")

	assertContains(t, scrape(t, registry),
		"# HELP users_online_total a simple counter",
		"# TYPE users_online_total counter",
		`users_online_total{country="usa"} 3`,
	)
}

func TestPrometheusExpositionKeepsOneNamePerCounter(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	for _, msg := range []string{
		"users.online:1|c|#country:china,city:beijing",
		"users.online:2|c|#country:usa,city:losangeles",
		"users.online:3|c|#country:usa",
	} {
		agg.Record(context.Background(), msg)
	}

	body := scrape(t, registry)
	assertContains(t, body,
		`users_online_total{city="beijing",country="china"} 1`,
		`users_online_total{city="losangeles",country="usa"} 2`,
		`users_online_total{country="usa"} 3`,
	)
	if strings.Contains(body, "_total_total") {
		t.Errorf("/metrics has a repeated _total suffix:\n%s", body)
	}
	if strings.Contains(body, "otel_scope_") {
		t.Errorf("/metrics has scope labels:\n%s", body)
	}
}

func TestInvalidUTF8IsNotRecordedAndScrapeSurvives(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	for _, msg := range []string{"foo\xff:1|c", "a\xff:1|c", "ok:1|c|#k:v\xff", "ok:1|c|#k\xff:v", "ok:1|c"} {
		agg.Record(context.Background(), msg)
	}

	assertContains(t, scrape(t, registry), "ok_total 1")
}

func TestInvalidTagKeyIsNotRecordedAndScrapeSurvives(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	for _, msg := range []string{
		"bad:1|c|#_:v",
		"bad:1|c|#.-:v",
		"bad:1|c|#é:v",
		"bad:1|c|#__name__:x",
		"bad:1|c|#ok:v,__x:y",
		"ok:1|c|#_k.é:v",
	} {
		agg.Record(context.Background(), msg)
	}

	body := scrape(t, registry)
	assertContains(t, body, `ok_total{_k_="v"} 1`)
	if strings.Contains(body, "bad_total") {
		t.Errorf("/metrics has a metric with an invalid tag key:\n%s", body)
	}
}

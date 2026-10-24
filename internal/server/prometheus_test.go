package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func newPrometheusAggregator(t testing.TB) (*Aggregator, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	provider, err := NewPrometheusMeterProvider(registry)
	if err != nil {
		t.Fatalf("NewPrometheusMeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return NewAggregator(provider.Meter("test"), discardLogger), registry
}

func scrape(t testing.TB, registry *prometheus.Registry) string {
	t.Helper()
	return scrapeWith(t, registry, promhttp.HandlerOpts{})
}

func scrapeWith(t testing.TB, registry *prometheus.Registry, opts promhttp.HandlerOpts) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(registry, opts))
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

func TestPrometheusGaugeExposition(t *testing.T) {
	t.Parallel()
	agg, registry := newPrometheusAggregator(t)
	for _, msg := range []string{
		"temperature:20|g|#room:a",
		"temperature:21.5|g|#room:a",
		"temperature:-3|g|#room:b",
		"temperature:19|g|@0.5|#room:c",
	} {
		agg.Record(context.Background(), msg)
	}

	body := scrape(t, registry)
	assertContains(t, body,
		"# HELP temperature a simple gauge",
		"# TYPE temperature gauge",
		`temperature{room="a"} 21.5`,
		`temperature{room="b"} -3`,
		`temperature{room="c"} 19`,
	)
	if strings.Contains(body, "temperature_total") {
		t.Errorf("/metrics has a _total suffix on a gauge:\n%s", body)
	}
}

// TestCounterAndGaugeWithTheSameNameKeepScrapesWorking pins what the exporter does when a
// counter and a gauge want the same Prometheus name. The names foo (gauge) and foo_total
// (counter) are different families and coexist. foo.total as a gauge collides with the foo
// counter: only the first instrument created is exposed, the exporter logs the conflict, and
// promhttp ContinueOnError, as configured in cmd/quicmetrics, keeps the rest of the scrape
// working.
// Without ContinueOnError the collision is an HTTP 500, like the foo and foo.total counters.
func TestCounterAndGaugeWithTheSameNameKeepScrapesWorking(t *testing.T) {
	t.Parallel()
	continueOnError := promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}

	t.Run("same name coexists", func(t *testing.T) {
		t.Parallel()
		agg, registry := newPrometheusAggregator(t)
		for _, msg := range []string{"foo:1|c", "foo:2|g", "foo:3|c", "foo:4|g"} {
			agg.Record(context.Background(), msg)
		}
		assertContains(t, scrape(t, registry),
			"# TYPE foo gauge", "foo 4",
			"# TYPE foo_total counter", "foo_total 4",
		)
	})

	for _, order := range [][]string{{"foo:1|c", "foo.total:2|g"}, {"foo.total:2|g", "foo:1|c"}} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			t.Parallel()
			agg, registry := newPrometheusAggregator(t)
			for _, msg := range order {
				agg.Record(context.Background(), msg)
			}
			agg.Record(context.Background(), "ok:1|c")

			body := scrapeWith(t, registry, continueOnError)
			assertContains(t, body, "ok_total 1")
			if n := strings.Count(body, "# TYPE foo_total "); n != 1 {
				t.Errorf("/metrics has %d foo_total families, want 1:\n%s", n, body)
			}
		})
	}

	t.Run("collision is an error without ContinueOnError", func(t *testing.T) {
		t.Parallel()
		agg, registry := newPrometheusAggregator(t)
		agg.Record(context.Background(), "foo:1|c")
		agg.Record(context.Background(), "foo.total:2|g")

		srv := httptest.NewServer(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
		}
	})
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

func seriesLine(i int) string {
	return fmt.Sprintf("bench.c%d:1|c|#function:f%d,region:r%d,id:%d", i%4, i%10, i%3, i)
}

func populateSeries(ctx context.Context, agg *Aggregator, series int) {
	for i := range series {
		agg.Record(ctx, seriesLine(i))
	}
}

func benchSeriesCount(tb testing.TB, registry *prometheus.Registry, prefix string) (count int, overflow bool) {
	tb.Helper()
	families, err := registry.Gather()
	if err != nil {
		tb.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), prefix) {
			continue
		}
		for _, m := range family.GetMetric() {
			count++
			for _, label := range m.GetLabel() {
				overflow = overflow || label.GetName() == "otel_metric_overflow"
			}
		}
	}
	return count, overflow
}

func BenchmarkRecordCardinality(b *testing.B) {
	ctx := context.Background()
	for _, series := range []int{1, 100, 1000, 5000} {
		b.Run(fmt.Sprintf("series_%d", series), func(b *testing.B) {
			lines := make([]string, series)
			for i := range lines {
				lines[i] = fmt.Sprintf("bench.requests:1|c|#id:%d", i)
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			agg, registry := newPrometheusAggregator(b)
			for _, line := range lines {
				agg.Record(ctx, line)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(agg)
			heapPerSeries := float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)) / float64(min(series, cardinalityLimit))

			i := 0
			for b.Loop() {
				agg.Record(ctx, lines[i])
				if i++; i == len(lines) {
					i = 0
				}
			}
			if series >= 100 {
				b.ReportMetric(heapPerSeries, "heap-B/series")
			}
			count, overflow := benchSeriesCount(b, registry, "bench_requests")
			if want := min(series, cardinalityLimit); count != want || overflow != (series > cardinalityLimit) {
				b.Fatalf("series = %d (overflow %t), want %d (overflow %t)", count, overflow, want, series > cardinalityLimit)
			}
		})
	}
}

func BenchmarkScrape(b *testing.B) {
	ctx := context.Background()
	for _, series := range []int{10, 100, 1000, 4000} {
		b.Run(fmt.Sprintf("series_%d", series), func(b *testing.B) {
			agg, registry := newPrometheusAggregator(b)
			populateSeries(ctx, agg, series)
			if count, _ := benchSeriesCount(b, registry, "bench_"); count != series {
				b.Fatalf("series = %d, want %d", count, series)
			}

			b.Run("gather", func(b *testing.B) {
				for b.Loop() {
					if _, err := registry.Gather(); err != nil {
						b.Fatalf("gather: %v", err)
					}
				}
			})
			b.Run("handler", func(b *testing.B) {
				handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
				req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bench_c0_total") {
					b.Fatalf("unexpected response, status %d:\n%s", rec.Code, rec.Body.String())
				}
				b.SetBytes(int64(rec.Body.Len()))
				for b.Loop() {
					handler.ServeHTTP(httptest.NewRecorder(), req)
				}
			})
		})
	}
}

// BenchmarkRecordWhileScraping compares ingestion alone with ingestion while a goroutine
// scrapes back to back, which is far more often than a real Prometheus does.
func BenchmarkRecordWhileScraping(b *testing.B) {
	const series = 1000
	lines := make([]string, series)
	for i := range lines {
		lines[i] = seriesLine(i)
	}
	ctx := context.Background()
	for _, scraping := range []bool{false, true} {
		name := "no_scrape"
		if scraping {
			name = "continuous_scrape"
		}
		b.Run(name, func(b *testing.B) {
			agg, registry := newPrometheusAggregator(b)
			populateSeries(ctx, agg, series)

			stop := make(chan struct{})
			var wg sync.WaitGroup
			var scrapes atomic.Int64
			if scraping {
				wg.Go(func() {
					for {
						select {
						case <-stop:
							return
						default:
						}
						if _, err := registry.Gather(); err != nil {
							b.Errorf("gather: %v", err)
							return
						}
						scrapes.Add(1)
					}
				})
			}

			var workers atomic.Int64
			b.RunParallel(func(pb *testing.PB) {
				i := int(workers.Add(1)) * 97
				for pb.Next() {
					agg.Record(ctx, lines[i%series])
					i++
				}
			})
			b.StopTimer()
			close(stop)
			wg.Wait()
			if scraping {
				b.ReportMetric(float64(scrapes.Load())/b.Elapsed().Seconds(), "scrapes/s")
			}
			if count, _ := benchSeriesCount(b, registry, "bench_"); count != series {
				b.Fatalf("series = %d, want %d", count, series)
			}
		})
	}
}

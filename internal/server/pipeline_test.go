package server

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func withProviderClock(now func() time.Time) ProviderOption {
	return func(c *providerConfig) { c.now = now }
}

const testTTL = 15 * time.Minute

type expiryEnv struct {
	agg      *Aggregator
	registry *prometheus.Registry
	provider *MeterProvider
	clock    *fakeClock
}

func newExpiryEnv(t testing.TB, ttl time.Duration, limits Limits) *expiryEnv {
	t.Helper()
	env := &expiryEnv{registry: prometheus.NewRegistry(), clock: &fakeClock{t: time.Unix(1_000_000, 0)}}
	provider, err := NewPrometheusMeterProvider(env.registry, WithSeriesTTL(ttl), withProviderClock(env.clock.Now))
	if err != nil {
		t.Fatalf("NewPrometheusMeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	env.provider = provider
	env.agg = NewAggregator(provider.Meter("test"), discardLogger, WithLimits(limits))
	return env
}

func (e *expiryEnv) record(lines ...string) {
	for _, line := range lines {
		e.agg.Record(context.Background(), line)
	}
}

func (e *expiryEnv) scrape(t testing.TB) string {
	t.Helper()
	return scrape(t, e.registry)
}

func lines(body, prefix string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

func assertAbsent(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("/metrics has %q:\n%s", u, body)
		}
	}
}

func TestSeriesDisappearAfterTheTTL(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, DefaultLimits())
	env.record(
		"hits:1|c|#fn:old", "hits:1|c|#fn:live",
		"temp:1|g|#fn:old", "temp:1|g|#fn:live",
		"lat:1|h|#fn:old", "lat:1|ms|#fn:live",
	)
	assertContains(t, env.scrape(t), `hits_total{fn="old"} 1`, `temp{fn="old"} 1`, `lat_count{fn="old"} 1`)

	env.clock.Advance(testTTL)
	env.record("hits:1|c|#fn:live", "temp:2|g|#fn:live", "lat:1|h|#fn:live")
	assertContains(t, env.scrape(t), `hits_total{fn="old"} 1`, `temp{fn="old"} 1`, `lat_count{fn="old"} 1`)

	env.clock.Advance(time.Second)
	body := env.scrape(t)
	assertAbsent(t, body, `fn="old"`)
	assertContains(t, body, `hits_total{fn="live"} 2`, `temp{fn="live"} 2`, `lat_count{fn="live"} 2`, `quicmetrics_series_expired_total 3`)
}

func TestExpiredSeriesRestartFromZero(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, DefaultLimits())
	env.record("hits:5|c")
	assertContains(t, env.scrape(t), "hits_total 5")
	env.clock.Advance(testTTL + time.Second)
	assertAbsent(t, env.scrape(t), "hits_total")
	env.record("hits:2|c")
	assertContains(t, env.scrape(t), "hits_total 2")
}

func TestSeriesAreKeptWhileTheyKeepReceivingMetrics(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, DefaultLimits())
	for i := 1; i <= 10; i++ {
		env.record("hits:1|c")
		env.clock.Advance(testTTL - time.Second)
		assertContains(t, env.scrape(t), fmt.Sprintf("hits_total %d", i))
	}
}

func TestOverflowSeriesApplyToUnexpiredSeriesOnly(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, Limits{})
	for i := range cardinalityLimit + 100 {
		env.record(fmt.Sprintf("hits:1|c|#id:%d", i))
	}
	body := env.scrape(t)
	if got := len(lines(body, "hits_total")); got != cardinalityLimit {
		t.Fatalf("%d series, want the limit of %d", got, cardinalityLimit)
	}
	assertContains(t, body, `hits_total{otel_metric_overflow="true"} 101`)

	env.clock.Advance(testTTL + time.Second)
	env.scrape(t)
	env.record("hits:1|c|#id:fresh")
	body = env.scrape(t)
	assertContains(t, body, `hits_total{id="fresh"} 1`)
	if got := len(lines(body, "hits_total")); got != 1 {
		t.Errorf("%d series after the others expired, want 1:\n%s", got, body)
	}
}

// BenchmarkScrapeWithSeriesTTL compares with BenchmarkScrape/gather: the producer copies every
// cumulative series on each scrape, and a scrape that follows updates also collects them.
func BenchmarkScrapeWithSeriesTTL(b *testing.B) {
	ctx := context.Background()
	for _, series := range []int{10, 100, 1000, 4000} {
		b.Run(fmt.Sprintf("series_%d", series), func(b *testing.B) {
			env := newExpiryEnv(b, testTTL, Limits{})
			populateSeries(ctx, env.agg, series)
			if count, _ := benchSeriesCount(b, env.registry, "bench_"); count != series {
				b.Fatalf("series = %d, want %d", count, series)
			}

			b.Run("gather", func(b *testing.B) {
				for b.Loop() {
					if _, err := env.registry.Gather(); err != nil {
						b.Fatalf("gather: %v", err)
					}
				}
			})
			b.Run("gather_after_100_updates", func(b *testing.B) {
				for b.Loop() {
					for i := range 100 {
						env.agg.Record(ctx, seriesLine(i))
					}
					if _, err := env.registry.Gather(); err != nil {
						b.Fatalf("gather: %v", err)
					}
				}
			})
		})
	}
}

func TestExpiredNamesKeepTheirSlot(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, Limits{MaxNames: 2})
	env.record("a:1|c|#t:1", "b:1|g", "c:1|c")
	env.scrape(t)
	env.clock.Advance(testTTL + time.Second)
	assertAbsent(t, env.scrape(t), "a_total", "b ")

	env.record("c:1|c", "a:1|c")
	body := env.scrape(t)
	assertContains(t, body, "a_total 1", `quicmetrics_metrics_dropped_total{reason="names"} 1`)
	assertAbsent(t, body, "\nc_total")
}

func TestAnExpiredCounterStillBlocksAGaugeWithTheSamePrometheusName(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, DefaultLimits())
	env.record("foo:1|c")
	assertContains(t, env.scrape(t), "foo_total 1")

	env.clock.Advance(testTTL + time.Second)
	env.scrape(t)
	env.record("foo.total:5|g", "ok:1|c")
	body := scrapeWith(t, env.registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
	assertContains(t, body, "ok_total 1")
	assertAbsent(t, body, "foo_total 5")
}

func TestDroppedMetricsExpireLikeOtherSeries(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, Limits{MaxTags: 1})
	env.record("hits:1|c|#a:1,b:2")
	assertContains(t, env.scrape(t), `quicmetrics_metrics_dropped_total{reason="tags"} 1`)
	env.clock.Advance(testTTL + time.Second)
	assertAbsent(t, env.scrape(t), "quicmetrics_metrics_dropped_total")
}

func TestScrapesLoseNoMeasurementWhileRecording(t *testing.T) {
	t.Parallel()
	env := newExpiryEnv(t, testTTL, DefaultLimits())
	const recorders, perRecorder = 4, 2000
	var wg sync.WaitGroup
	for range recorders {
		wg.Go(func() {
			for range perRecorder {
				env.record("steady:1|c", "steady.h:3|h")
			}
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for scraping := true; scraping; {
		select {
		case <-done:
			scraping = false
		default:
			env.clock.Advance(time.Second)
			env.scrape(t)
			runtime.Gosched()
		}
	}
	assertContains(t, env.scrape(t),
		fmt.Sprintf("steady_total %d", recorders*perRecorder),
		fmt.Sprintf("steady_h_count %d", recorders*perRecorder),
		fmt.Sprintf("steady_h_sum %d", 3*recorders*perRecorder))
}

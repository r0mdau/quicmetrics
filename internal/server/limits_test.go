package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func withClock(now func() time.Time) AggregatorOption {
	return func(a *Aggregator) { a.now = now }
}

func newLimitedAggregator(limits Limits, opts ...AggregatorOption) (*Aggregator, sdkmetric.Reader) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	return NewAggregator(meter, discardLogger, append([]AggregatorOption{WithLimits(limits)}, opts...)...), reader
}

func collectDropped(t testing.TB, reader sdkmetric.Reader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	dropped := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != droppedMetricName {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s: unexpected aggregation %T", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				reason, _ := dp.Attributes.Value("reason")
				dropped[reason.AsString()] = dp.Value
			}
		}
	}
	return dropped
}

func assertDropped(t *testing.T, reader sdkmetric.Reader, want map[string]int64) {
	t.Helper()
	got := collectDropped(t, reader)
	if len(got) != len(want) {
		t.Fatalf("dropped = %v, want %v", got, want)
	}
	for reason, n := range want {
		if got[reason] != n {
			t.Errorf("dropped{reason=%q} = %d, want %d (all: %v)", reason, got[reason], n, got)
		}
	}
}

func TestNameLimitDropsNewInstrumentsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agg, reader := newLimitedAggregator(Limits{MaxNames: 3})
	for _, raw := range []string{
		"a:1|c", "b:1|c", "a:1.5|g",
		"c:1|c",
		"a:2|g",
		"a:2|c",
		"b:1|h",
		"b:1|ms",
		"c:3|g|#k:v",
		"a:5|c|#k:v",
		"b:7|c|@0.5",
		"d.e:1|s",
		"c:1|c|#__x:",
	} {
		agg.Record(ctx, raw)
	}

	assertValues(t, values(collectSums(t, reader)), map[string]float64{
		"a{}": 3, "b{}": 15, "a{k=v}": 5,
	})
	assertValues(t, collectGauges(t, reader), map[string]float64{"a{}": 2})
	if h := collectHistograms(t, reader); len(h) != 0 {
		t.Errorf("histograms = %v, want none", h)
	}
	assertDropped(t, reader, map[string]int64{"names": 4})
}

func TestNameLimitCountsHistogramsAndTimersOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agg, reader := newLimitedAggregator(Limits{MaxNames: 2})
	for _, raw := range []string{"lat:1|h", "lat:2|ms", "lat:3|h|#a:b", "other:1|c", "third:1|c"} {
		agg.Record(ctx, raw)
	}
	assertHistograms(t, collectHistograms(t, reader), map[string]histogramWant{
		"lat{}":    {count: 2, sum: 3, buckets: map[float64]uint64{5: 2}},
		"lat{a=b}": {count: 1, sum: 3, buckets: map[float64]uint64{5: 1}},
	})
	assertDropped(t, reader, map[string]int64{"names": 1})
}

func TestMetricsDroppedForTheirTagsDoNotTakeANameSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agg, reader := newLimitedAggregator(Limits{MaxNames: 1, MaxTags: 1})
	agg.Record(ctx, "greedy:1|c|#a:1,b:2")
	agg.Record(ctx, "9lives:1|c")
	agg.Record(ctx, "fine:1|c")
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"fine{}": 1})
	assertDropped(t, reader, map[string]int64{"tags": 1, "invalid_name": 1})
}

func TestZeroLimitsDisableTheChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agg, reader := newLimitedAggregator(Limits{})
	long := strings.Repeat("x", 5000)
	var tags []string
	for i := range 500 {
		tags = append(tags, fmt.Sprintf("k%d:%s", i, long))
	}
	for i := range 3000 {
		agg.Record(ctx, fmt.Sprintf("name%d:1|c", i))
	}
	agg.Record(ctx, "tagged:1|c|#"+strings.Join(tags, ","))
	if got := len(collectSums(t, reader)); got != 3001 {
		t.Errorf("recorded %d series, want 3001", got)
	}
	assertDropped(t, reader, map[string]int64{})
}

func TestTagLimits(t *testing.T) {
	t.Parallel()
	limits := Limits{MaxTags: 3, MaxTagKeyLen: 5, MaxTagValueLen: 6}
	tests := []struct {
		name   string
		raw    string
		series string // empty when the metric must be dropped
		reason string
	}{
		{"at every limit", "m:1|c|#abcde:123456,b:2,c:3", "m{abcde=123456,b=2,c=3}", ""},
		{"too many tags", "m:1|c|#a:1,b:2,c:3,d:4", "", "tags"},
		{"key too long", "m:1|c|#abcdef:1", "", "tag_key_length"},
		{"value too long", "m:1|c|#a:1234567", "", "tag_value_length"},
		{"multibyte value counted in bytes", "m:1|c|#a:éééé", "", "tag_value_length"},
		{"multibyte value within limit", "m:1|c|#a:ééé", "m{a=ééé}", ""},
		{"gauge too many tags", "m:1|g|#a:1,b:2,c:3,d:4", "", "tags"},
		{"histogram key too long", "m:1|h|#abcdef:1", "", "tag_key_length"},
		{"timer value too long", "m:1|ms|#a:1234567", "", "tag_value_length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg, reader := newLimitedAggregator(limits)
			agg.Record(context.Background(), tt.raw)
			sums, gauges, histograms := collect(t, reader)
			recorded := len(sums) + len(gauges) + len(histograms)
			if tt.series == "" {
				if recorded != 0 {
					t.Errorf("recorded %v %v %v, want nothing", sums, gauges, histograms)
				}
				assertDropped(t, reader, map[string]int64{tt.reason: 1})
				return
			}
			if _, ok := sums[tt.series]; !ok || recorded != 1 {
				t.Errorf("recorded %v %v %v, want only %s", sums, gauges, histograms, tt.series)
			}
			assertDropped(t, reader, map[string]int64{})
		})
	}
}

func TestReservedNamesAreDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agg, reader := newLimitedAggregator(Limits{})
	for _, raw := range []string{
		"quicmetrics.metrics.dropped:5|c|#reason:names",
		"quicmetrics_metrics_dropped:5|c",
		"quicmetrics.metrics.dropped.total:5|g",
		"quicmetrics-x:1|h",
		"quicmetrics/x:1|ms",
	} {
		agg.Record(ctx, raw)
	}
	for _, raw := range []string{"quicmetrics:1|c", "quicmetricsx:1|c", "Quicmetrics.x:1|c", "my.quicmetrics.x:1|c"} {
		agg.Record(ctx, raw)
	}
	assertValues(t, values(collectSums(t, reader)), map[string]float64{
		"quicmetrics{}": 1, "quicmetricsx{}": 1, "Quicmetrics.x{}": 1, "my.quicmetrics.x{}": 1,
	})
	assertDropped(t, reader, map[string]int64{"reserved_name": 5})
}

func TestValidInstrumentNameMatchesTheSDK(t *testing.T) {
	t.Parallel()
	meter := sdkmetric.NewMeterProvider().Meter("test")
	for _, name := range []string{
		"", "a", "A", "z9", "a.b-c_d/e", "9a", "_a", ".a", "-a", "/a", "a b", "a:b", "a,b", "a\x00", "é",
		"aé", "éa", "a|b", "a@b", "a#b", strings.Repeat("a", 255), strings.Repeat("a", 256),
	} {
		_, err := meter.Float64Counter(name)
		if got, want := validInstrumentName(name), err == nil; got != want {
			t.Errorf("validInstrumentName(%q) = %v, but the SDK says %v (%v)", name, got, want, err)
		}
	}
}

func TestInvalidNamesDoNotTakeNameSlots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agg, reader := newLimitedAggregator(Limits{MaxNames: 1})
	for i := range 100 {
		agg.Record(ctx, fmt.Sprintf("%d-bad:1|c", i))
	}
	agg.Record(ctx, "good:1|c")
	assertValues(t, values(collectSums(t, reader)), map[string]float64{"good{}": 1})
	assertDropped(t, reader, map[string]int64{"invalid_name": 100})
}

func TestDropLogsAreRateLimitedPerReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	now := time.Unix(1_000, 0)
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	agg := NewAggregator(meter, logger, WithLimits(Limits{MaxNames: 1, MaxTags: 1}),
		withClock(func() time.Time { return now }))

	agg.Record(ctx, "keep:1|c")
	lines := func() []string { return strings.Split(strings.TrimSpace(logs.String()), "\n") }

	for i := range 1000 {
		agg.Record(ctx, fmt.Sprintf("flood%d:1|c", i))
	}
	if got := lines(); len(got) != 1 {
		t.Fatalf("1000 drops logged %d lines, want 1:\n%s", len(got), logs.String())
	}
	for _, want := range []string{"level=WARN", "reason=names", "limit=1", "dropped=1", "example=flood0"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}

	agg.Record(ctx, "tagged:1|c|#a:1,b:2")
	if got := lines(); len(got) != 2 || !strings.Contains(got[1], "reason=tags") {
		t.Fatalf("another reason should log right away:\n%s", logs.String())
	}

	now = now.Add(dropLogInterval - time.Second)
	agg.Record(ctx, "flood:1|c")
	if got := lines(); len(got) != 2 {
		t.Fatalf("logged again before the interval:\n%s", logs.String())
	}
	now = now.Add(time.Second)
	agg.Record(ctx, "flood:1|c")
	got := lines()
	if len(got) != 3 || !strings.Contains(got[2], "dropped=1001") {
		t.Fatalf("want a third line counting the 1001 drops since the first:\n%s", logs.String())
	}
	assertDropped(t, reader, map[string]int64{"names": 1002, "tags": 1})
}

func TestDropLogsCapTheClientControlledName(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	meter := sdkmetric.NewMeterProvider().Meter("test")
	agg := NewAggregator(meter, logger, WithLimits(Limits{MaxTags: 1}))

	name := strings.Repeat("é", 100)
	agg.Record(context.Background(), name+":1|c|#a:1,b:2")
	if strings.Contains(logs.String(), name) {
		t.Errorf("log has the whole name:\n%s", logs.String())
	}
	if want := strings.Repeat("é", maxLoggedName/2); !strings.Contains(logs.String(), want) {
		t.Errorf("log missing the first %d bytes of the name:\n%s", maxLoggedName, logs.String())
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"aéb", 2, "a"},
		{"aéb", 3, "aé"},
		{"é", 0, ""},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestNameLimitHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const limit = 50
	agg, reader := newLimitedAggregator(Limits{MaxNames: limit})
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				agg.Record(ctx, fmt.Sprintf("n%d:1|c", (w*31+i)%200))
				agg.Record(ctx, fmt.Sprintf("n%d:1|g", (w*17+i)%200))
				agg.Record(ctx, fmt.Sprintf("n%d:1|h", (w*7+i)%200))
			}
		})
	}
	wg.Wait()
	sums, gauges, histograms := collect(t, reader)
	if got := len(sums) + len(gauges) + len(histograms); got != limit {
		t.Errorf("%d instruments with one series each, want exactly %d", got, limit)
	}
	if collectDropped(t, reader)["names"] == 0 {
		t.Error("no metric was counted as dropped")
	}
}

func TestDroppedMetricsAreExposedOnTheRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registry := prometheus.NewRegistry()
	provider, err := NewPrometheusMeterProvider(registry)
	if err != nil {
		t.Fatalf("NewPrometheusMeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	agg := NewAggregator(provider.Meter("test"), discardLogger, WithLimits(Limits{MaxNames: 1, MaxTags: 1}))

	agg.Record(ctx, "kept:1|c")
	agg.Record(ctx, "second:1|c")
	agg.Record(ctx, "third:1|g")
	agg.Record(ctx, "kept:1|c|#a:1,b:2")
	agg.Record(ctx, "quicmetrics.metrics.dropped:100|c")

	body := scrape(t, registry)
	assertContains(t, body,
		"# TYPE quicmetrics_metrics_dropped_total counter",
		`quicmetrics_metrics_dropped_total{reason="names"} 2`,
		`quicmetrics_metrics_dropped_total{reason="tags"} 1`,
		`quicmetrics_metrics_dropped_total{reason="reserved_name"} 1`,
		"kept_total 1",
	)
	if strings.Contains(body, "second_total") || strings.Contains(body, "third") {
		t.Errorf("/metrics exposes a metric over the name limit:\n%s", body)
	}
}

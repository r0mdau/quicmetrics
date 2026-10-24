package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/narqo/go-dogstatsd-parser"
	"go.opentelemetry.io/otel/attribute"
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

func collectSums(t testing.TB, reader sdkmetric.Reader) map[string]metricdata.Sum[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
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
		{name: "NaN counter", raw: "foo:NaN|c", wantErr: true},
		{name: "infinite counter", raw: "foo:+Inf|c", wantErr: true},
		{name: "negative infinite counter", raw: "foo:-Inf|c", wantErr: true},
		{name: "counter above 2^63", raw: "foo:1e19|c", wantErr: true},
		{name: "counter below -2^63", raw: "foo:-1e19|c", wantErr: true},
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

func TestParseCounterValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want float64
	}{
		{raw: "foo:1|c", want: 1},
		{raw: "foo:1.5|c", want: 1.5},
		{raw: "foo:0.25|c|@0.5|#a:b", want: 0.25},
		{raw: "foo:1e-3|c", want: 0.001},
		{raw: "foo:9223372036854775808|c", want: 1 << 63},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			m, err := Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.raw, err)
			}
			var got float64
			switch v := m.Value.(type) {
			case int64:
				got = float64(v)
			case float64:
				got = v
			default:
				t.Fatalf("Parse(%q) value = %v (%T), want a number", tt.raw, m.Value, m.Value)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) value = %v, want %v", tt.raw, got, tt.want)
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
			name:     "fractional values are kept",
			messages: []string{"foo:1.5|c", "foo:0.25|c"},
			want:     map[string]float64{"foo{}": 1.75},
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
		"foo:NaN|c",
		"foo:Inf|c",
		"foo:1e308|c",
		"foo:1|x",
		"9lives:1|c",
		"foo@bar:1|c",
		"foo:1|c|#_:v",
		"foo:1|c|#__name__:x",
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

// benchLines returns count valid counters of value 1 shaped like the client generator, spread
// over a few hundred series so no counter gets near the cardinality limit.
func benchLines(tb testing.TB, count int) []string {
	tb.Helper()
	var (
		functions = []string{"checkout", "auth", "search", "resize-image", "send-email"}
		regions   = []string{"us-east-1", "eu-west-1", "ap-southeast-1"}
		methods   = []string{"GET", "POST", "PUT", "DELETE"}
		statuses  = []string{"200", "201", "404", "500", "503"}
		errorKind = []string{"timeout", "unhandled", "oom"}
	)
	rng := rand.New(rand.NewPCG(1, 2))
	pick := func(choices []string) string { return choices[rng.IntN(len(choices))] }
	lines := make([]string, count)
	for i := range lines {
		switch rng.IntN(4) {
		case 0:
			lines[i] = fmt.Sprintf("faas.invocations:1|c|#function:%s,region:%s,cold_start:%t",
				pick(functions), pick(regions), rng.IntN(20) == 0)
		case 1:
			lines[i] = fmt.Sprintf("http.requests:1|c|#function:%s,region:%s,method:%s,status:%s",
				pick(functions), pick(regions), pick(methods), pick(statuses))
		case 2:
			lines[i] = fmt.Sprintf("faas.billed.ms:1|c|#function:%s,region:%s", pick(functions), pick(regions))
		default:
			lines[i] = fmt.Sprintf("faas.errors:1|c|#function:%s,region:%s,error:%s",
				pick(functions), pick(regions), pick(errorKind))
		}
		if m, err := Parse(lines[i]); err != nil || m.Type != "c" {
			tb.Fatalf("benchLines produced an unusable line %q: %v", lines[i], err)
		}
	}
	return lines
}

func taggedLine(tags int) string {
	var sb strings.Builder
	sb.WriteString("bench.requests:1|c")
	for i := range tags {
		if i == 0 {
			sb.WriteString("|#")
		} else {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "tag%d:value%d", i, i)
	}
	return sb.String()
}

func sumValues(values map[string]float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum
}

func BenchmarkParse(b *testing.B) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "counter_4_tags", raw: taggedLine(4)},
		{name: "empty_segment", raw: "foo:1|c|", wantErr: true},
		{name: "invalid_utf8", raw: "foo\xff:1|c", wantErr: true},
		{name: "garbage", raw: "garbage", wantErr: true},
	}
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			if _, err := Parse(tt.raw); (err != nil) != tt.wantErr {
				b.Fatalf("Parse(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			for b.Loop() {
				_, _ = Parse(tt.raw)
			}
		})
	}
}

func BenchmarkRecord(b *testing.B) {
	infoLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tests := []struct {
		name       string
		raw        string
		logger     *slog.Logger
		wantSeries int
	}{
		{"tags_0", taggedLine(0), discardLogger, 1},
		{"tags_4", taggedLine(4), discardLogger, 1},
		{"tags_8", taggedLine(8), discardLogger, 1},
		{"tags_4_info_logger", taggedLine(4), infoLogger, 1},
		{"gauge_dropped", "bench.temperature:21.5|g", infoLogger, 0},
		{"invalid_warned", "foo:1|c|", infoLogger, 0},
	}
	ctx := context.Background()
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			reader := sdkmetric.NewManualReader()
			agg := NewAggregator(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("bench"), tt.logger)
			agg.Record(ctx, tt.raw)
			recorded := 1
			for b.Loop() {
				agg.Record(ctx, tt.raw)
				recorded++
			}
			got := values(collectSums(b, reader))
			if len(got) != tt.wantSeries {
				b.Fatalf("got series %v, want %d", got, tt.wantSeries)
			}
			if tt.wantSeries > 0 && sumValues(got) != float64(recorded) {
				b.Fatalf("recorded total = %v, want %d", sumValues(got), recorded)
			}
		})
	}
}

func BenchmarkRecordParallel(b *testing.B) {
	lines := benchLines(b, 1024)
	agg, reader := newTestAggregator()
	ctx := context.Background()
	var workers, recorded atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		i := int(workers.Add(1)) * 97
		var n int64
		for pb.Next() {
			agg.Record(ctx, lines[i%len(lines)])
			i++
			n++
		}
		recorded.Add(n)
	})
	if got := sumValues(values(collectSums(b, reader))); got != float64(recorded.Load()) {
		b.Fatalf("recorded total = %v, want %d", got, recorded.Load())
	}
}

// TestRecordAllocations guards the hot path against allocation regressions. Unlike timings,
// allocation counts are stable enough to fail a build. It is not parallel because
// testing.AllocsPerRun counts every allocation of the process.
func TestRecordAllocations(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		raw       string
		maxAllocs float64
	}{
		{"counter_4_tags", taggedLine(4), 14},
		// One more than an integer: Parse boxes the float64 that replaces the parser's int64.
		{"float_counter_4_tags", strings.Replace(taggedLine(4), ":1|", ":1.5|", 1), 15},
		{"gauge_dropped", "bench.temperature:21.5|g", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg, _ := newTestAggregator()
			agg.Record(ctx, tt.raw)
			if got := testing.AllocsPerRun(100, func() { agg.Record(ctx, tt.raw) }); got > tt.maxAllocs {
				t.Errorf("Record(%q) = %v allocs/op, want at most %v", tt.raw, got, tt.maxAllocs)
			}
		})
	}
}

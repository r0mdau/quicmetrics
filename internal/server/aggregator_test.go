package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
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

// collect returns the sums and gauges recorded so far, keyed by name and attributes. It fails
// on any other aggregation, so a metric that ends up with an unexpected kind is not skipped.
func collect(t testing.TB, reader sdkmetric.Reader) (map[string]metricdata.Sum[float64], map[string]float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	sums := map[string]metricdata.Sum[float64]{}
	gauges := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, dp := range data.DataPoints {
					sums[seriesKey(m.Name, dp.Attributes)] = metricdata.Sum[float64]{
						DataPoints:  []metricdata.DataPoint[float64]{dp},
						Temporality: data.Temporality,
						IsMonotonic: data.IsMonotonic,
					}
				}
			case metricdata.Gauge[float64]:
				for _, dp := range data.DataPoints {
					gauges[seriesKey(m.Name, dp.Attributes)] = dp.Value
				}
			default:
				t.Fatalf("metric %q: unexpected aggregation %T", m.Name, m.Data)
			}
		}
	}
	return sums, gauges
}

func seriesKey(name string, attrs attribute.Set) string {
	return name + "{" + attrs.Encoded(attribute.DefaultEncoder()) + "}"
}

func collectSums(t testing.TB, reader sdkmetric.Reader) map[string]metricdata.Sum[float64] {
	t.Helper()
	sums, _ := collect(t, reader)
	return sums
}

func collectGauges(t testing.TB, reader sdkmetric.Reader) map[string]float64 {
	t.Helper()
	_, gauges := collect(t, reader)
	return gauges
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
		{name: "gauge with tags", raw: "temperature:21.5|g|#room:a"},
		{name: "negative gauge", raw: "temperature:-3|g"},
		{name: "huge gauge", raw: "temperature:1e300|g"},
		{name: "gauge above 2^63", raw: "temperature:1e19|g"},
		{name: "NaN gauge", raw: "temperature:NaN|g", wantErr: true},
		{name: "infinite gauge", raw: "temperature:+Inf|g", wantErr: true},
		{name: "negative infinite gauge", raw: "temperature:-Inf|g", wantErr: true},
		{name: "overflowing gauge", raw: "temperature:1e999|g", wantErr: true},
		{name: "empty gauge segment panics in the parser", raw: "temperature:1|g|", wantErr: true},
		{name: "invalid UTF-8 gauge", raw: "temperature\xff:1|g", wantErr: true},
		{name: "sampled gauge", raw: "temperature:21.5|g|@0.1|#room:a"},
		{name: "gauge sample rate 1", raw: "temperature:21.5|g|@1"},
		{name: "gauge sample rate 0", raw: "temperature:21.5|g|@0", wantErr: true},
		{name: "gauge NaN sample rate", raw: "temperature:21.5|g|@NaN", wantErr: true},
		{name: "gauge sample rate above 1", raw: "temperature:21.5|g|@1.5", wantErr: true},
		{name: "gauge negative sample rate", raw: "temperature:21.5|g|@-0.5", wantErr: true},
		{name: "gauge sample rate not a number", raw: "temperature:21.5|g|@abc", wantErr: true},
		{name: "gauge sample rate after tags", raw: "temperature:21.5|g|#a:b|@0", wantErr: true},
		{name: "gauge later sample rate 0 after a valid one", raw: "temperature:21.5|g|@0.5|@0", wantErr: true},
		{name: "gauge sample rate underflowing float32 is judged as float64", raw: "temperature:21.5|g|@1e-50"},
		{name: "gauge smallest sample rate is valid since nothing is scaled", raw: "temperature:21.5|g|@5e-324"},
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
		{name: "sampled counter", raw: "foo:1|c|@0.1|#a:b"},
		{name: "sample rate 1", raw: "foo:1|c|@1"},
		{name: "sample rate 0", raw: "foo:1|c|@0", wantErr: true},
		{name: "sample rate 0 on a zero counter", raw: "foo:0|c|@0", wantErr: true},
		{name: "negative zero sample rate", raw: "foo:1|c|@-0", wantErr: true},
		{name: "NaN sample rate", raw: "foo:1|c|@NaN", wantErr: true},
		{name: "infinite sample rate", raw: "foo:1|c|@+Inf", wantErr: true},
		{name: "sample rate above 1", raw: "foo:1|c|@1.5", wantErr: true},
		{name: "negative sample rate", raw: "foo:1|c|@-0.5", wantErr: true},
		{name: "sample rate not a number", raw: "foo:1|c|@abc", wantErr: true},
		{name: "empty sample rate", raw: "foo:1|c|@", wantErr: true},
		{name: "later sample rate 0 after a valid one", raw: "foo:1|c|@0.5|@0", wantErr: true},
		{name: "sample rate after tags", raw: "foo:1|c|#a:b|@0", wantErr: true},
		{name: "sample rate underflowing float32", raw: "foo:1|c|@1e-50", wantErr: true},
		{name: "smallest sample rate overflows the sum", raw: "foo:1|c|@5e-324", wantErr: true},
		{name: "scaled counter above 2^63", raw: "foo:1e18|c|@0.01", wantErr: true},
		{name: "scaled counter below -2^63", raw: "foo:-1e18|c|@0.01", wantErr: true},
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
		{raw: "foo:0.25|c|@0.5|#a:b", want: 0.5},
		{raw: "foo:1e-3|c", want: 0.001},
		{raw: "foo:9223372036854775808|c", want: 1 << 63},
		{raw: "foo:1|c|@0.1", want: 10},
		{raw: "foo:3|c|@0.3", want: 10},
		{raw: "foo:1|c|@0.01", want: 100},
		{raw: "foo:1|c|@0.5", want: 2},
		{raw: "foo:1|c|@1", want: 1},
		{raw: "foo:1.5|c|@0.25", want: 6},
		{raw: "foo:-3|c|@0.5", want: -6},
		{raw: "foo:0|c|@0.1", want: 0},
		{raw: "foo:4|c|#a:b|@0.5", want: 8},
		{raw: "foo:1|c|@0.5|@0.25", want: 4},
		{raw: "foo:4611686018427387904|c|@0.5", want: 1 << 63},
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

func TestParseGaugeValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want float64
	}{
		{raw: "temperature:21.5|g", want: 21.5},
		{raw: "temperature:3|g", want: 3},
		{raw: "temperature:+3|g", want: 3},
		{raw: "temperature:-3|g", want: -3},
		{raw: "temperature:0|g", want: 0},
		{raw: "temperature:1e-3|g", want: 0.001},
		{raw: "temperature:1e300|g", want: 1e300},
		{raw: "temperature:21.5|g|@0.1", want: 21.5},
		{raw: "temperature:21.5|g|@0.5|#a:b", want: 21.5},
		{raw: "temperature:21.5|g|#a:b|@0.25", want: 21.5},
		{raw: "temperature:21.5|g|@5e-324", want: 21.5},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			m, err := Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.raw, err)
			}
			if m.Type != dogstatsd.Gauge {
				t.Errorf("Parse(%q) type = %q, want a gauge", tt.raw, m.Type)
			}
			if got, ok := m.Value.(float64); !ok || got != tt.want {
				t.Errorf("Parse(%q) value = %v (%T), want float64 %v", tt.raw, m.Value, m.Value, tt.want)
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
			name:     "sampled counter is scaled by 1/rate",
			messages: []string{"foo:1|c|@0.1"},
			want:     map[string]float64{"foo{}": 10},
		},
		{
			name:     "sample rate applies to the value and keeps the tags",
			messages: []string{"foo:2|c|@0.5|#a:x", "foo:3|c|#a:x|@0.25"},
			want:     map[string]float64{"foo{a=x}": 16},
		},
		{
			name:     "sampled and unsampled counters share a series",
			messages: []string{"foo:1|c|@0.1", "foo:1|c", "foo:1|c|@1"},
			want:     map[string]float64{"foo{}": 12},
		},
		{
			name:     "many sampled events add up to the real count",
			messages: slices.Repeat([]string{"foo:1|c|@0.1"}, 100),
			want:     map[string]float64{"foo{}": 1000},
		},
		{
			name:     "invalid sample rates drop the metric and leave the series alone",
			messages: []string{"foo:1|c", "foo:1|c|@0", "foo:1|c|@NaN", "foo:1|c|@1e-50", "foo:1e18|c|@0.01", "foo:1|c|@2"},
			want:     map[string]float64{"foo{}": 1},
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

func TestGaugeAggregation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		messages []string
		want     map[string]float64
	}{
		{
			name:     "single gauge without tags",
			messages: []string{"temperature:21.5|g"},
			want:     map[string]float64{"temperature{}": 21.5},
		},
		{
			name:     "tags become attributes",
			messages: []string{"temperature:21.5|g|#room:a,floor:2"},
			want:     map[string]float64{"temperature{floor=2,room=a}": 21.5},
		},
		{
			name:     "last value wins per series",
			messages: []string{"temperature:20|g|#room:a", "temperature:25|g|#room:a", "temperature:18|g|#room:a"},
			want:     map[string]float64{"temperature{room=a}": 18},
		},
		{
			name:     "tag sets are separate series",
			messages: []string{"temperature:20|g|#room:a", "temperature:25|g|#room:b", "temperature:30|g", "temperature:21|g|#room:a"},
			want: map[string]float64{
				"temperature{room=a}": 21,
				"temperature{room=b}": 25,
				"temperature{}":       30,
			},
		},
		{
			name:     "different names are separate metrics",
			messages: []string{"temperature:20|g", "humidity:55|g"},
			want:     map[string]float64{"temperature{}": 20, "humidity{}": 55},
		},
		{
			name:     "fractional values are kept",
			messages: []string{"temperature:0.25|g", "load:1e-3|g"},
			want:     map[string]float64{"temperature{}": 0.25, "load{}": 0.001},
		},
		{
			name:     "negative values are kept",
			messages: []string{"temperature:5|g", "temperature:-12.5|g"},
			want:     map[string]float64{"temperature{}": -12.5},
		},
		{
			name:     "zero overwrites",
			messages: []string{"temperature:5|g", "temperature:0|g"},
			want:     map[string]float64{"temperature{}": 0},
		},
		{
			name:     "a leading sign is an absolute value, not a delta",
			messages: []string{"temperature:10|g", "temperature:+3|g"},
			want:     map[string]float64{"temperature{}": 3},
		},
		{
			name:     "a negative sign is an absolute value, not a delta",
			messages: []string{"temperature:10|g", "temperature:-3|g"},
			want:     map[string]float64{"temperature{}": -3},
		},
		{
			name:     "values above 2^63 are fine since nothing is summed",
			messages: []string{"temperature:1e300|g"},
			want:     map[string]float64{"temperature{}": 1e300},
		},
		{
			name:     "sample rate is ignored",
			messages: []string{"temperature:21.5|g|@0.1", "load:3|g|@0.5|#a:x"},
			want:     map[string]float64{"temperature{}": 21.5, "load{a=x}": 3},
		},
		{
			name:     "sampled and unsampled gauges share a series",
			messages: []string{"temperature:20|g|@0.1", "temperature:22|g", "temperature:24|g|@1"},
			want:     map[string]float64{"temperature{}": 24},
		},
		{
			name:     "invalid gauges leave the series alone",
			messages: []string{"temperature:20|g", "temperature:NaN|g", "temperature:+Inf|g", "temperature:1|g|@0", "temperature:1|g|#__x:y"},
			want:     map[string]float64{"temperature{}": 20},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg, reader := newTestAggregator()
			for _, msg := range tt.messages {
				agg.Record(context.Background(), msg)
			}
			assertValues(t, collectGauges(t, reader), tt.want)
			if sums := collectSums(t, reader); len(sums) != 0 {
				t.Errorf("recorded sums %v, want none", values(sums))
			}
		})
	}
}

func TestCountersAndGaugesAreSeparateInstruments(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	for _, msg := range []string{"requests:5|c", "temperature:21.5|g", "requests:2|c", "temperature:19|g"} {
		agg.Record(context.Background(), msg)
	}

	assertValues(t, values(collectSums(t, reader)), map[string]float64{"requests{}": 7})
	assertValues(t, collectGauges(t, reader), map[string]float64{"temperature{}": 19})
}

func TestInvalidGaugeRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		"temperature:NaN|g",
		"temperature:Inf|g",
		"temperature:+Inf|g",
		"temperature:-Inf|g",
		"temperature:1e999|g",
		"temperature:abc|g",
		"temperature:|g",
		"temperature:21.5|g|",
		"temperature:21.5|g||#a:b",
		"temperature:21.5|g|@0",
		"temperature:21.5|g|@-0",
		"temperature:21.5|g|@NaN",
		"temperature:21.5|g|@-0.5",
		"temperature:21.5|g|@1.5",
		"temperature:21.5|g|@abc",
		"temperature:21.5|g|@",
		"temperature:21.5|g|#a:b|@0",
		"temperature:21.5|g|#_:v",
		"temperature:21.5|g|#__name__:x",
		"temperature:21.5|g|#ok:v,__x:y",
		"temperature:21.5|g|#é:v",
		"temperature\xff:21.5|g",
		"temperature:21.5|g|#k:v\xff",
		"9lives:21.5|g",
		"foo@bar:21.5|g",
		"-foo:21.5|g",
	} {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()
			agg, reader := newTestAggregator()
			agg.Record(context.Background(), msg)
			sums, gauges := collect(t, reader)
			if len(sums) != 0 || len(gauges) != 0 {
				t.Errorf("recorded %v and %v, want nothing", values(sums), gauges)
			}
		})
	}
}

func TestIgnoredInputRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		"temperature:21.5|h",
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
		"foo:1|c|@0",
		"foo:1|c|@NaN",
		"foo:1|c|@-0.5",
		"foo:1|c|@1.5",
		"foo:1|c|@abc",
		"foo:1|c|@1e-50",
		"foo:1e18|c|@0.01",
		"temperature:21.5|h|@0",
		"",
	} {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()
			agg, reader := newTestAggregator()
			agg.Record(context.Background(), msg)
			sums, gauges := collect(t, reader)
			if len(sums) != 0 || len(gauges) != 0 {
				t.Errorf("recorded %v and %v, want nothing", values(sums), gauges)
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

func TestRecordGaugeValueTypes(t *testing.T) {
	t.Parallel()
	agg, reader := newTestAggregator()
	ctx := context.Background()
	agg.recordGauge(ctx, &dogstatsd.Metric{Type: dogstatsd.Gauge, Name: "floats", Value: 1.5})
	agg.recordGauge(ctx, &dogstatsd.Metric{Type: dogstatsd.Gauge, Name: "ints", Value: int64(2)})
	agg.recordGauge(ctx, &dogstatsd.Metric{Type: dogstatsd.Gauge, Name: "strings", Value: "x"})
	agg.recordGauge(ctx, &dogstatsd.Metric{Type: dogstatsd.Gauge, Name: "nils"})

	assertValues(t, collectGauges(t, reader), map[string]float64{"floats{}": 1.5})
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

func gaugeLine(tags int) string {
	return strings.Replace(taggedLine(tags), "bench.requests:1|c", "bench.temperature:21.5|g", 1)
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
		{name: "gauge_4_tags", raw: gaugeLine(4)},
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
		gauge      bool
	}{
		{"tags_0", taggedLine(0), discardLogger, 1, false},
		{"tags_4", taggedLine(4), discardLogger, 1, false},
		{"tags_8", taggedLine(8), discardLogger, 1, false},
		{"tags_4_info_logger", taggedLine(4), infoLogger, 1, false},
		{"gauge_tags_4", gaugeLine(4), discardLogger, 1, true},
		{"histogram_dropped", "bench.latency:12|h", infoLogger, 0, false},
		{"invalid_warned", "foo:1|c|", infoLogger, 0, false},
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
			sums, gauges := collect(b, reader)
			got := values(sums)
			if tt.gauge {
				if len(sums) != 0 || len(gauges) != tt.wantSeries || sumValues(gauges) != 21.5*float64(tt.wantSeries) {
					b.Fatalf("got sums %v and gauges %v, want %d gauge series of 21.5", got, gauges, tt.wantSeries)
				}
				return
			}
			if len(got) != tt.wantSeries || len(gauges) != 0 {
				b.Fatalf("got series %v and gauges %v, want %d sums", got, gauges, tt.wantSeries)
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
		// Parse boxes the scaled float64 just like a fractional value.
		{"sampled_counter_4_tags", strings.Replace(taggedLine(4), ":1|c", ":1|c|@0.1", 1), 15},
		// The parser already boxes the float64, so a gauge costs as much as an integer counter.
		{"gauge_4_tags", gaugeLine(4), 14},
		{"histogram_dropped", "bench.latency:12|h", 7},
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

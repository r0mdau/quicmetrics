package server

import (
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var accStart = time.Unix(1_000, 0)

func tagSet(v string) attribute.Set { return attribute.NewSet(attribute.String("k", v)) }

func deltaSum(name string, points ...metricdata.DataPoint[float64]) *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{{
		Name: name, Description: "d", Unit: "u",
		Data: metricdata.Sum[float64]{Temporality: metricdata.DeltaTemporality, IsMonotonic: true, DataPoints: points},
	}}}}}
}

func deltaGauge(name string, points ...metricdata.DataPoint[float64]) *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{{
		Name: name, Data: metricdata.Gauge[float64]{DataPoints: points},
	}}}}}
}

func deltaHistogram(name string, bounds []float64, points ...metricdata.HistogramDataPoint[float64]) *metricdata.ResourceMetrics {
	for i := range points {
		points[i].Bounds = bounds
	}
	return &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{{
		Name: name, Data: metricdata.Histogram[float64]{Temporality: metricdata.DeltaTemporality, DataPoints: points},
	}}}}}
}

func point(attrs attribute.Set, v float64) metricdata.DataPoint[float64] {
	return metricdata.DataPoint[float64]{Attributes: attrs, StartTime: accStart, Time: accStart, Value: v}
}

func snapshotSums(t *testing.T, a *accumulator, now time.Time, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, m := range a.snapshot(now) {
		if m.Name != name {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[float64])
		if !ok || sum.Temporality != metricdata.CumulativeTemporality || !sum.IsMonotonic {
			t.Fatalf("%s: got %T %+v, want a cumulative monotonic sum", name, m.Data, m.Data)
		}
		for _, dp := range sum.DataPoints {
			out[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
		}
	}
	return out
}

func TestAccumulatorSumsCounterDeltas(t *testing.T) {
	t.Parallel()
	a := newAccumulator(0, time.Minute)
	a.merge(deltaSum("c", point(tagSet("a"), 2), point(tagSet("b"), 3)), accStart)
	a.merge(deltaSum("c", point(tagSet("a"), 4)), accStart.Add(time.Second))
	assertValues(t, snapshotSums(t, a, accStart.Add(2*time.Second), "c"), map[string]float64{"k=a": 6, "k=b": 3})

	m := a.snapshot(accStart)[0]
	if m.Description != "d" || m.Unit != "u" {
		t.Errorf("snapshot = %q %q, want description and unit of the delta", m.Description, m.Unit)
	}
}

func TestAccumulatorKeepsTheLastGaugeValue(t *testing.T) {
	t.Parallel()
	a := newAccumulator(0, time.Minute)
	a.merge(deltaGauge("g", point(tagSet("a"), 1), point(tagSet("b"), 5)), accStart)
	a.merge(deltaGauge("g", point(tagSet("a"), -2)), accStart.Add(time.Second))
	got := map[string]float64{}
	for _, dp := range a.snapshot(accStart)[0].Data.(metricdata.Gauge[float64]).DataPoints {
		got[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
	}
	assertValues(t, got, map[string]float64{"k=a": -2, "k=b": 5})
}

func TestAccumulatorAddsHistogramDeltas(t *testing.T) {
	t.Parallel()
	bounds := []float64{5, 10}
	hp := func(count uint64, sum float64, buckets ...uint64) metricdata.HistogramDataPoint[float64] {
		return metricdata.HistogramDataPoint[float64]{
			Attributes: tagSet("a"), StartTime: accStart, Count: count, Sum: sum, BucketCounts: buckets,
		}
	}
	a := newAccumulator(0, time.Minute)
	a.merge(deltaHistogram("h", bounds, hp(2, 7, 1, 1, 0)), accStart)
	a.merge(deltaHistogram("h", bounds, hp(1, 50, 0, 0, 1)), accStart.Add(time.Second))
	a.merge(deltaHistogram("h", []float64{1}, hp(9, 9, 9, 0)), accStart.Add(2*time.Second))
	a.merge(deltaHistogram("h", bounds, hp(9, 9, 9)), accStart.Add(3*time.Second))

	dps := a.snapshot(accStart)[0].Data.(metricdata.Histogram[float64]).DataPoints
	if len(dps) != 1 {
		t.Fatalf("got %d series, want 1: %+v", len(dps), dps)
	}
	dp := dps[0]
	if dp.Count != 3 || dp.Sum != 57 || fmt.Sprint(dp.BucketCounts) != "[1 1 1]" || fmt.Sprint(dp.Bounds) != "[5 10]" {
		t.Errorf("histogram = count %d, sum %v, buckets %v, bounds %v; want 3, 57, [1 1 1], [5 10]",
			dp.Count, dp.Sum, dp.BucketCounts, dp.Bounds)
	}
}

func TestAccumulatorSnapshotIsIndependent(t *testing.T) {
	t.Parallel()
	a := newAccumulator(0, time.Minute)
	hp := metricdata.HistogramDataPoint[float64]{Attributes: tagSet("a"), Count: 1, BucketCounts: []uint64{1, 0}}
	a.merge(deltaHistogram("h", []float64{5}, hp), accStart)
	first := a.snapshot(accStart)[0].Data.(metricdata.Histogram[float64]).DataPoints[0]
	a.merge(deltaHistogram("h", []float64{5}, hp), accStart)
	if first.Count != 1 || first.BucketCounts[0] != 1 {
		t.Errorf("an earlier snapshot changed: %+v", first)
	}
}

func TestAccumulatorExpiresIdleSeriesAndEmptyInstruments(t *testing.T) {
	t.Parallel()
	const ttl = time.Minute
	a := newAccumulator(0, ttl)
	a.merge(deltaSum("c", point(tagSet("old"), 1), point(tagSet("live"), 1)), accStart)
	a.merge(deltaSum("only", point(tagSet("x"), 1)), accStart)
	a.merge(deltaSum("c", point(tagSet("live"), 1)), accStart.Add(ttl))

	if got := a.expire(accStart.Add(ttl)); got != 0 {
		t.Fatalf("expire at exactly the TTL forgot %d series, want none", got)
	}
	if got := a.expire(accStart.Add(ttl + time.Second)); got != 2 {
		t.Fatalf("expire forgot %d series, want 2", got)
	}
	assertValues(t, snapshotSums(t, a, accStart, "c"), map[string]float64{"k=live": 2})
	if _, ok := a.instruments[accKey{kindCounter, "only"}]; ok {
		t.Error("instrument without series is still held")
	}
}

func TestAccumulatorLimitsSeriesLikeTheSDK(t *testing.T) {
	t.Parallel()
	const limit = 5
	a := newAccumulator(limit, time.Minute)
	var points []metricdata.DataPoint[float64]
	for i := range 10 {
		points = append(points, point(tagSet(fmt.Sprint(i)), 1))
	}
	a.merge(deltaSum("c", points...), accStart)
	got := snapshotSums(t, a, accStart, "c")
	if len(got) != limit {
		t.Fatalf("kept %d series, want %d: %v", len(got), limit, got)
	}
	if got["otel.metric.overflow=true"] != 6 {
		t.Errorf("overflow = %v, want the 6 measurements that did not fit (all: %v)", got["otel.metric.overflow=true"], got)
	}

	a.merge(deltaSum("c", point(tagSet("0"), 1), point(tagSet("new"), 1), point(overflowSet, 10)), accStart.Add(time.Second))
	got = snapshotSums(t, a, accStart, "c")
	if len(got) != limit || got["k=0"] != 2 || got["otel.metric.overflow=true"] != 17 {
		t.Errorf("after more points = %v, want k=0 at 2 and the overflow at 17 over %d series", got, limit)
	}

	a.merge(deltaSum("c", point(tagSet("late"), 1)), accStart.Add(time.Hour))
	if got := a.expire(accStart.Add(time.Hour)); got != limit-1 {
		t.Errorf("expire forgot %d series, want the %d idle ones", got, limit-1)
	}
	a.merge(deltaSum("c", point(tagSet("fresh"), 1)), accStart.Add(time.Hour+time.Second))
	assertValues(t, snapshotSums(t, a, accStart, "c"), map[string]float64{"k=fresh": 1, "otel.metric.overflow=true": 18})
}

func TestAccumulatorAddsInt64Counters(t *testing.T) {
	t.Parallel()
	a := newAccumulator(0, time.Minute)
	delta := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{{
		Name: "i", Data: metricdata.Sum[int64]{Temporality: metricdata.DeltaTemporality, IsMonotonic: true,
			DataPoints: []metricdata.DataPoint[int64]{{Attributes: tagSet("a"), Value: 2}}},
	}}}}}
	a.merge(delta, accStart)
	a.merge(delta, accStart)
	assertValues(t, snapshotSums(t, a, accStart, "i"), map[string]float64{"k=a": 4})
}

func TestAccumulatorIgnoresOtherAggregations(t *testing.T) {
	t.Parallel()
	a := newAccumulator(0, time.Minute)
	a.merge(&metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{
		{Name: "i", Data: metricdata.Histogram[int64]{DataPoints: []metricdata.HistogramDataPoint[int64]{{Count: 1}}}},
	}}}}, accStart)
	if got := a.snapshot(accStart); len(got) != 0 {
		t.Errorf("snapshot = %+v, want nothing", got)
	}
}

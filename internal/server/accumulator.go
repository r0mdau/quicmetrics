package server

import (
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// overflowSet is the attribute set the SDK gives the measurements that don't fit its cardinality
// limit, and the accumulator uses it for its own.
var overflowSet = attribute.NewSet(attribute.Bool("otel.metric.overflow", true))

type metricKind int

const (
	kindCounter metricKind = iota
	kindGauge
	kindHistogram
)

type accKey struct {
	kind metricKind
	name string
}

// accumulator turns the delta metrics of successive collections into cumulative ones and forgets
// the series no delta touched for ttl, which the SDK can't do for a cumulative instrument.
type accumulator struct {
	limit       int
	ttl         time.Duration
	instruments map[accKey]*accInstrument
}

type accInstrument struct {
	name        string
	description string
	unit        string
	bounds      []float64
	series      map[attribute.Distinct]*accSeries
}

// accSeries holds a counter's total or a gauge's last value in value, and a histogram's
// count, sum and buckets.
type accSeries struct {
	attrs   attribute.Set
	start   time.Time
	seen    time.Time
	value   float64
	count   uint64
	sum     float64
	buckets []uint64
}

func newAccumulator(limit int, ttl time.Duration) *accumulator {
	return &accumulator{limit: limit, ttl: ttl, instruments: map[accKey]*accInstrument{}}
}

func (a *accumulator) instrument(kind metricKind, m metricdata.Metrics) *accInstrument {
	key := accKey{kind, m.Name}
	inst, ok := a.instruments[key]
	if !ok {
		inst = &accInstrument{name: m.Name, description: m.Description, unit: m.Unit, series: map[attribute.Distinct]*accSeries{}}
		a.instruments[key] = inst
	}
	return inst
}

// get returns the series of attrs, or the overflow series when the instrument is full. Like the
// SDK, it keeps room for the overflow series, which does not count against the others, so
// where the delta puts its own overflow point doesn't change how many series fit.
func (a *accumulator) get(inst *accInstrument, attrs attribute.Set, start, now time.Time) *accSeries {
	key := attrs.Equivalent()
	s, ok := inst.series[key]
	if !ok && a.limit > 0 && key != overflowSet.Equivalent() && a.full(inst) {
		attrs, key = overflowSet, overflowSet.Equivalent()
		s, ok = inst.series[key]
	}
	if !ok {
		s = &accSeries{attrs: attrs, start: start}
		inst.series[key] = s
	}
	s.seen = now
	return s
}

func (a *accumulator) full(inst *accInstrument) bool {
	n := len(inst.series)
	if _, overflowing := inst.series[overflowSet.Equivalent()]; overflowing {
		n--
	}
	return n >= a.limit-1
}

// merge adds the delta metrics of one collection, and marks the series they touch as seen at
// now. It ignores everything but counters, gauges and histograms.
func (a *accumulator) merge(rm *metricdata.ResourceMetrics, now time.Time) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[float64]:
				inst := a.instrument(kindCounter, m)
				for _, dp := range data.DataPoints {
					a.get(inst, dp.Attributes, dp.StartTime, now).value += dp.Value
				}
			case metricdata.Sum[int64]:
				inst := a.instrument(kindCounter, m)
				for _, dp := range data.DataPoints {
					a.get(inst, dp.Attributes, dp.StartTime, now).value += float64(dp.Value)
				}
			case metricdata.Gauge[float64]:
				inst := a.instrument(kindGauge, m)
				for _, dp := range data.DataPoints {
					a.get(inst, dp.Attributes, dp.StartTime, now).value = dp.Value
				}
			case metricdata.Histogram[float64]:
				inst := a.instrument(kindHistogram, m)
				for _, dp := range data.DataPoints {
					a.mergeHistogram(inst, dp, now)
				}
			}
		}
	}
}

func (a *accumulator) mergeHistogram(inst *accInstrument, dp metricdata.HistogramDataPoint[float64], now time.Time) {
	if inst.bounds == nil {
		inst.bounds = slices.Clone(dp.Bounds)
	}
	if !slices.Equal(inst.bounds, dp.Bounds) || len(dp.BucketCounts) != len(inst.bounds)+1 {
		return
	}
	s := a.get(inst, dp.Attributes, dp.StartTime, now)
	if s.buckets == nil {
		s.buckets = make([]uint64, len(dp.BucketCounts))
	}
	for b, n := range dp.BucketCounts {
		s.buckets[b] += n
	}
	s.count += dp.Count
	s.sum += dp.Sum
}

// expire forgets the series not seen for more than the TTL before now, and the instruments left
// without series. It returns the number of series forgotten.
func (a *accumulator) expire(now time.Time) (forgotten int) {
	for key, inst := range a.instruments {
		for k, s := range inst.series {
			if now.Sub(s.seen) > a.ttl {
				delete(inst.series, k)
				forgotten++
			}
		}
		if len(inst.series) == 0 {
			delete(a.instruments, key)
		}
	}
	return forgotten
}

// snapshot returns the cumulative metrics at now, sharing nothing with the accumulator.
func (a *accumulator) snapshot(now time.Time) []metricdata.Metrics {
	out := make([]metricdata.Metrics, 0, len(a.instruments))
	for key, inst := range a.instruments {
		m := metricdata.Metrics{Name: inst.name, Description: inst.description, Unit: inst.unit}
		switch key.kind {
		case kindCounter:
			m.Data = metricdata.Sum[float64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: true, DataPoints: inst.points(now)}
		case kindGauge:
			m.Data = metricdata.Gauge[float64]{DataPoints: inst.points(now)}
		case kindHistogram:
			dps := make([]metricdata.HistogramDataPoint[float64], 0, len(inst.series))
			for _, s := range inst.series {
				dps = append(dps, metricdata.HistogramDataPoint[float64]{
					Attributes: s.attrs, StartTime: s.start, Time: now,
					Count: s.count, Sum: s.sum, Bounds: inst.bounds, BucketCounts: slices.Clone(s.buckets),
				})
			}
			m.Data = metricdata.Histogram[float64]{Temporality: metricdata.CumulativeTemporality, DataPoints: dps}
		}
		out = append(out, m)
	}
	return out
}

func (inst *accInstrument) points(now time.Time) []metricdata.DataPoint[float64] {
	dps := make([]metricdata.DataPoint[float64], 0, len(inst.series))
	for _, s := range inst.series {
		dps = append(dps, metricdata.DataPoint[float64]{Attributes: s.attrs, StartTime: s.start, Time: now, Value: s.value})
	}
	return dps
}

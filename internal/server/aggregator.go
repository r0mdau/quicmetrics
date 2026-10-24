package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/narqo/go-dogstatsd-parser"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Parse parses a single DogStatsD line. Unlike dogstatsd.Parse it never panics, and it
// rejects input that is not valid UTF-8 because client_golang rejects such label values, which
// would break every later scrape. Counter values must be numbers of at most 2^63 in magnitude,
// since a NaN or infinite sum would stick to the series forever. They are not truncated:
// values that don't fit an int64 exactly, such as 1.5, are returned as float64.
//
// The returned counter value is already divided by the sample rate, so foo:1|c|@0.1 yields
// 10. The scaled value obeys the same bound, and a rate outside (0, 1] is an error. Rate keeps
// the float32 the parser read and must not be applied again.
//
// Gauge values are returned as the parser read them, as float64. They must be finite, since
// nothing is summed there is no other bound, and a sign (+3, -3) is part of the absolute value.
// The sample rate must be in (0, 1] like for counters, but it is not applied to the value.
func Parse(raw string) (*dogstatsd.Metric, error) {
	if !utf8.ValidString(raw) {
		return nil, errors.New("metric is not valid UTF-8")
	}
	m, err := parse(raw)
	if err != nil {
		return nil, err
	}
	switch m.Type {
	case dogstatsd.Counter:
		v, err := counterValue(raw)
		if err != nil {
			return nil, err
		}
		// Keeping the parser's int64 when it is exact avoids boxing a second value.
		if i, ok := m.Value.(int64); !ok || float64(i) != v {
			m.Value = v
		}
	case dogstatsd.Gauge:
		if err := checkGauge(m, raw); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func parse(raw string) (m *dogstatsd.Metric, err error) {
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("parser panic: %v", r)
		}
	}()
	return dogstatsd.Parse(raw)
}

// counterValue parses the value of a counter line that dogstatsd.Parse already accepted and
// scales it by its sample rate.
func counterValue(raw string) (float64, error) {
	s, tail := valueAndTail(raw)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse counter value: %w", err)
	}
	if math.IsNaN(v) || math.Abs(v) > maxCounterValue {
		return 0, fmt.Errorf("counter value %q is not a number of at most 2^63 in magnitude", s)
	}
	rate, err := sampleRate(tail)
	if err != nil {
		return 0, err
	}
	scaled := v / rate
	if math.IsNaN(scaled) || math.Abs(scaled) > maxCounterValue {
		return 0, fmt.Errorf("counter value %q at sample rate %v is not a number of at most 2^63 in magnitude", s, rate)
	}
	return scaled, nil
}

// checkGauge validates a gauge that dogstatsd.Parse already accepted: its value must be finite
// and its sample rate valid, although the rate is not applied.
func checkGauge(m *dogstatsd.Metric, raw string) error {
	if v, ok := m.Value.(float64); !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("gauge value %v is not a finite number", m.Value)
	}
	_, tail := valueAndTail(raw)
	_, err := sampleRate(tail)
	return err
}

// valueAndTail splits a line that dogstatsd.Parse already accepted into the raw value and
// the segments that follow the value.
func valueAndTail(raw string) (value, tail string) {
	_, rest, _ := strings.Cut(raw, ":")
	value, tail, _ = strings.Cut(rest, "|")
	return value, tail
}

// sampleRate returns the last @rate segment of the part of a line that follows the value, or 1
// when there is none. It reads the rate as a float64 because the parser's float32 turns @0.1
// into 0.10000000149, and it accepts only (0, 1]: the parser lets 0 and NaN through, and
// dividing by them would give an infinite or NaN sum.
func sampleRate(tail string) (float64, error) {
	rate := 1.0
	for seg := range strings.SplitSeq(tail, "|") {
		raw, ok := strings.CutPrefix(seg, "@")
		if !ok {
			continue
		}
		r, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("parse sample rate: %w", err)
		}
		if !(r > 0 && r <= 1) {
			return 0, fmt.Errorf("sample rate %q is not in (0, 1]", raw)
		}
		rate = r
	}
	return rate, nil
}

// maxCounterValue bounds each counter value so that summing them can't realistically
// overflow to +Inf, which would stick to the series as much as a NaN.
const maxCounterValue = 1 << 63

// Aggregator records DogStatsD counters and gauges as OpenTelemetry instruments.
type Aggregator struct {
	meter  metric.Meter
	logger *slog.Logger
}

// NewAggregator returns an Aggregator that records into meter and logs to logger.
func NewAggregator(meter metric.Meter, logger *slog.Logger) *Aggregator {
	return &Aggregator{meter: meter, logger: logger}
}

// Record parses raw and records it when it is a counter or a gauge. Invalid lines and other
// metric types are logged and dropped.
func (a *Aggregator) Record(ctx context.Context, raw string) {
	a.logger.Debug("metric received", "raw", raw)

	m, err := Parse(raw)
	if err != nil {
		a.logger.Warn("invalid metric", "raw", raw, "err", err)
		return
	}
	switch m.Type {
	case dogstatsd.Counter:
		a.recordCounter(ctx, m)
	case dogstatsd.Gauge:
		a.recordGauge(ctx, m)
	default:
		a.logger.Debug("metric type not aggregated", "raw", raw, "type", string(m.Type))
	}
}

func (a *Aggregator) recordCounter(ctx context.Context, m *dogstatsd.Metric) {
	var value float64
	switch v := m.Value.(type) {
	case int64:
		value = float64(v)
	case float64:
		value = v
	default:
		a.logger.Warn("unsupported counter value", "name", m.Name, "type", fmt.Sprintf("%T", m.Value))
		return
	}

	attrs, ok := a.attributes(m)
	if !ok {
		return
	}
	counter, err := a.meter.Float64Counter(m.Name, metric.WithDescription("a simple counter"))
	if err != nil {
		a.logger.Warn("create counter", "name", m.Name, "err", err)
		return
	}
	counter.Add(ctx, value, metric.WithAttributes(attrs...))
}

func (a *Aggregator) recordGauge(ctx context.Context, m *dogstatsd.Metric) {
	value, ok := m.Value.(float64)
	if !ok {
		a.logger.Warn("unsupported gauge value", "name", m.Name, "type", fmt.Sprintf("%T", m.Value))
		return
	}

	attrs, ok := a.attributes(m)
	if !ok {
		return
	}
	gauge, err := a.meter.Float64Gauge(m.Name, metric.WithDescription("a simple gauge"))
	if err != nil {
		a.logger.Warn("create gauge", "name", m.Name, "err", err)
		return
	}
	gauge.Record(ctx, value, metric.WithAttributes(attrs...))
}

// attributes turns the tags of m into attributes. It logs and reports false when a tag key
// can't become a Prometheus label name, in which case the whole metric must be dropped.
func (a *Aggregator) attributes(m *dogstatsd.Metric) ([]attribute.KeyValue, bool) {
	attrs := make([]attribute.KeyValue, 0, len(m.Tags))
	for k, v := range m.Tags {
		if !validTagKey(k) {
			a.logger.Warn("invalid tag key", "name", m.Name, "key", k)
			return nil, false
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs, true
}

// validTagKey reports whether k becomes a usable Prometheus label name: it needs an ASCII
// letter or digit, and the `__` prefix is reserved by Prometheus.
func validTagKey(k string) bool {
	return !strings.HasPrefix(k, "__") && strings.ContainsFunc(k, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	})
}

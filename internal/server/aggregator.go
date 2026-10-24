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
func Parse(raw string) (*dogstatsd.Metric, error) {
	if !utf8.ValidString(raw) {
		return nil, errors.New("metric is not valid UTF-8")
	}
	m, err := parse(raw)
	if err != nil {
		return nil, err
	}
	if m.Type == dogstatsd.Counter {
		v, err := counterValue(raw)
		if err != nil {
			return nil, err
		}
		// Keeping the parser's int64 when it is exact avoids boxing a second value.
		if i, ok := m.Value.(int64); !ok || float64(i) != v {
			m.Value = v
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

// counterValue parses the value of a counter line that dogstatsd.Parse already accepted.
func counterValue(raw string) (float64, error) {
	_, rest, _ := strings.Cut(raw, ":")
	s, _, _ := strings.Cut(rest, "|")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse counter value: %w", err)
	}
	if math.IsNaN(v) || math.Abs(v) > maxCounterValue {
		return 0, fmt.Errorf("counter value %q is not a number of at most 2^63 in magnitude", s)
	}
	return v, nil
}

// maxCounterValue bounds each counter value so that summing them can't realistically
// overflow to +Inf, which would stick to the series as much as a NaN.
const maxCounterValue = 1 << 63

// Aggregator records DogStatsD counters as OpenTelemetry instruments.
type Aggregator struct {
	meter  metric.Meter
	logger *slog.Logger
}

// NewAggregator returns an Aggregator that records into meter and logs to logger.
func NewAggregator(meter metric.Meter, logger *slog.Logger) *Aggregator {
	return &Aggregator{meter: meter, logger: logger}
}

// Record parses raw and records it when it is a counter. Invalid lines and other metric
// types are logged and dropped.
func (a *Aggregator) Record(ctx context.Context, raw string) {
	a.logger.Debug("metric received", "raw", raw)

	m, err := Parse(raw)
	if err != nil {
		a.logger.Warn("invalid metric", "raw", raw, "err", err)
		return
	}
	if m.Type != dogstatsd.Counter {
		a.logger.Debug("metric type not aggregated", "raw", raw, "type", string(m.Type))
		return
	}
	a.recordCounter(ctx, m)
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

	for k := range m.Tags {
		if !validTagKey(k) {
			a.logger.Warn("invalid tag key", "name", m.Name, "key", k)
			return
		}
	}

	counter, err := a.meter.Float64Counter(m.Name, metric.WithDescription("a simple counter"))
	if err != nil {
		a.logger.Warn("create counter", "name", m.Name, "err", err)
		return
	}
	attrs := make([]attribute.KeyValue, 0, len(m.Tags))
	for k, v := range m.Tags {
		attrs = append(attrs, attribute.String(k, v))
	}
	counter.Add(ctx, value, metric.WithAttributes(attrs...))
}

// validTagKey reports whether k becomes a usable Prometheus label name: it needs an ASCII
// letter or digit, and the `__` prefix is reserved by Prometheus.
func validTagKey(k string) bool {
	return !strings.HasPrefix(k, "__") && strings.ContainsFunc(k, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	})
}

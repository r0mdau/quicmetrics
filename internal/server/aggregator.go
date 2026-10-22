package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/narqo/go-dogstatsd-parser"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/instrument"
)

// Parse parses a single DogStatsD line. Unlike dogstatsd.Parse it never panics, and it
// rejects input that is not valid UTF-8 because the Prometheus exporter panics on such names.
func Parse(raw string) (m *dogstatsd.Metric, err error) {
	if !utf8.ValidString(raw) {
		return nil, errors.New("metric is not valid UTF-8")
	}
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("parser panic: %v", r)
		}
	}()
	return dogstatsd.Parse(raw)
}

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

	counter, err := a.meter.SyncFloat64().Counter(m.Name, instrument.WithDescription("a simple counter"))
	if err != nil {
		a.logger.Warn("create counter", "name", m.Name, "err", err)
		return
	}
	attrs := make([]attribute.KeyValue, 0, len(m.Tags))
	for k, v := range m.Tags {
		attrs = append(attrs, attribute.Key(k).String(v))
	}
	counter.Add(ctx, value, attrs...)
}

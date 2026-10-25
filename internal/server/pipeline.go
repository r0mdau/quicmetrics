package server

import (
	"context"
	"errors"
	"sync"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const expiredMetricName = "quicmetrics.series.expired"

// MeterProvider is the provider NewPrometheusMeterProvider returns: the SDK provider the
// Aggregator records into, and with a series TTL the one that hosts the exporter.
type MeterProvider struct {
	*sdkmetric.MeterProvider
	exporter *sdkmetric.MeterProvider
}

// Shutdown shuts down the providers and the exporter.
func (p *MeterProvider) Shutdown(ctx context.Context) error {
	err := p.MeterProvider.Shutdown(ctx)
	if p.exporter != nil {
		err = errors.Join(err, p.exporter.Shutdown(ctx))
	}
	return err
}

// pipeline is the Producer of the exporter when series expire. Clients record into a provider
// with delta temporality, whose reader only returns the series updated since the last scrape.
// Each scrape adds that delta to the accumulator, which forgets the idle series.
type pipeline struct {
	reader sdkmetric.Reader
	now    func() time.Time
	start  time.Time

	mu      sync.Mutex
	acc     *accumulator
	expired int64
}

// deltaTemporality also covers gauges, which sdkmetric.DeltaTemporalitySelector keeps cumulative.
func deltaTemporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.DeltaTemporality
}

func newPipeline(ttl time.Duration, limit int, now func() time.Time) *pipeline {
	return &pipeline{
		reader: sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(deltaTemporality)),
		now:    now,
		start:  now(),
		acc:    newAccumulator(limit, ttl),
	}
}

// Produce implements sdkmetric.Producer.
func (p *pipeline) Produce(ctx context.Context) ([]metricdata.ScopeMetrics, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	var rm metricdata.ResourceMetrics
	err := p.reader.Collect(ctx, &rm)
	p.acc.merge(&rm, now)
	p.expired += int64(p.acc.expire(now))

	metrics := p.acc.snapshot(now)
	if p.expired > 0 {
		metrics = append(metrics, metricdata.Metrics{
			Name:        expiredMetricName,
			Description: "series forgotten because no metric updated them for the series TTL",
			Data: metricdata.Sum[int64]{
				Temporality: metricdata.CumulativeTemporality,
				IsMonotonic: true,
				DataPoints:  []metricdata.DataPoint[int64]{{StartTime: p.start, Time: now, Value: p.expired}},
			},
		})
	}
	return []metricdata.ScopeMetrics{{Metrics: metrics}}, err
}

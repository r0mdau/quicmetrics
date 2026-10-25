package server

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// ProviderOption configures NewPrometheusMeterProvider.
type ProviderOption func(*providerConfig)

type providerConfig struct {
	seriesTTL time.Duration
	now       func() time.Time
}

// WithSeriesTTL makes a series disappear once no metric updated it for ttl, and come back from
// zero if it is updated again. A TTL of 0 or less keeps every series.
func WithSeriesTTL(ttl time.Duration) ProviderOption {
	return func(c *providerConfig) { c.seriesTTL = ttl }
}

// DefaultSeriesTTL is the series TTL of the server command.
const DefaultSeriesTTL = 15 * time.Minute

// NewPrometheusMeterProvider returns a MeterProvider whose metrics are exported to
// registerer. Names use Prometheus conventions (`users.online` becomes
// `users_online_total`), and scope labels are omitted so series only carry DogStatsD tags.
// Each counter, gauge and histogram keeps at most cardinalityLimit tag sets; later ones are
// aggregated into a single series labeled `otel_metric_overflow="true"`, which sums a counter's
// values, holds a gauge's last overflowing value and merges a histogram's observations. With
// WithSeriesTTL, the limit applies to the series that have not expired.
func NewPrometheusMeterProvider(registerer prometheus.Registerer, opts ...ProviderOption) (*MeterProvider, error) {
	cfg := providerConfig{now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	exporterOpts := []otelprom.Option{
		otelprom.WithRegisterer(registerer),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		otelprom.WithoutScopeInfo(),
	}
	var pipe *pipeline
	if cfg.seriesTTL > 0 {
		pipe = newPipeline(cfg.seriesTTL, cardinalityLimit, cfg.now)
		exporterOpts = append(exporterOpts, otelprom.WithProducer(pipe))
	}
	exporter, err := otelprom.New(exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("create Prometheus exporter: %w", err)
	}
	if pipe == nil {
		return &MeterProvider{MeterProvider: sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(exporter),
			sdkmetric.WithCardinalityLimit(cardinalityLimit),
		)}, nil
	}
	return &MeterProvider{
		MeterProvider: sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(pipe.reader),
			sdkmetric.WithCardinalityLimit(cardinalityLimit),
		),
		exporter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter)),
	}, nil
}

const cardinalityLimit = 2000

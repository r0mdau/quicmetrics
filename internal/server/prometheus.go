package server

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// NewPrometheusMeterProvider returns a MeterProvider whose metrics are exported to
// registerer. Names use Prometheus conventions (`users.online` becomes
// `users_online_total`), and scope labels are omitted so series only carry DogStatsD tags.
// Each counter, gauge and histogram keeps at most cardinalityLimit tag sets; later ones are
// aggregated into a single series labeled `otel_metric_overflow="true"`, which sums a counter's
// values, holds a gauge's last overflowing value and merges a histogram's observations.
func NewPrometheusMeterProvider(registerer prometheus.Registerer) (*sdkmetric.MeterProvider, error) {
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registerer),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("create Prometheus exporter: %w", err)
	}
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithCardinalityLimit(cardinalityLimit),
	), nil
}

const cardinalityLimit = 2000

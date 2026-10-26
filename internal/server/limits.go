package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Limits bounds what a client can make an Aggregator keep. A limit of 0 or less disables its check.
type Limits struct {
	MaxNames       int
	MaxTags        int
	MaxTagKeyLen   int
	MaxTagValueLen int
}

// DefaultLimits returns the limits of an Aggregator without WithLimits.
func DefaultLimits() Limits {
	return Limits{MaxNames: 1000, MaxTags: 20, MaxTagKeyLen: 64, MaxTagValueLen: 200}
}

const (
	droppedMetricName = "quicmetrics.metrics.dropped"
	reservedPrefix    = "quicmetrics"
	dropLogInterval   = time.Minute
	maxLoggedName     = 64
)

const (
	dropNames          = "names"
	dropTags           = "tags"
	dropTagKeyLength   = "tag_key_length"
	dropTagValueLength = "tag_value_length"
	dropReservedName   = "reserved_name"
	dropInvalidName    = "invalid_name"
	dropLineTooLong    = "line_too_long"
	dropSlowClient     = "slow_client"
)

func reservedName(name string) bool {
	rest, ok := strings.CutPrefix(name, reservedPrefix)
	return ok && rest != "" && strings.ContainsRune("._-/", rune(rest[0]))
}

type drops struct {
	counter metric.Int64Counter
	logger  *slog.Logger
	now     func() time.Time

	mu     sync.Mutex
	states map[string]dropState
}

type dropState struct {
	dropped int64
	last    time.Time
}

func newDrops(meter metric.Meter, logger *slog.Logger, now func() time.Time) *drops {
	counter, err := meter.Int64Counter(droppedMetricName,
		metric.WithDescription("metrics dropped because of a limit, by reason"))
	if err != nil {
		logger.Error("create dropped metrics counter", "err", err)
	}
	return &drops{counter: counter, logger: logger, now: now, states: map[string]dropState{}}
}

func (d *drops) record(ctx context.Context, reason, example string, limit int) {
	d.counter.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))

	d.mu.Lock()
	now := d.now()
	s := d.states[reason]
	s.dropped++
	dropped := s.dropped
	quiet := now.Sub(s.last) < dropLogInterval
	if !quiet {
		s.dropped, s.last = 0, now
	}
	d.states[reason] = s
	d.mu.Unlock()
	if quiet {
		return
	}

	attrs := []any{"reason", reason, "dropped", dropped, "example", truncate(example, maxLoggedName)}
	if limit > 0 {
		attrs = append(attrs, "limit", limit)
	}
	d.logger.Warn("metrics dropped", attrs...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var (
	errNamesFull   = errors.New("too many distinct metric names")
	errInvalidName = errors.New("invalid instrument name")
)

// validInstrumentName mirrors the SDK's check, which registers an instrument before rejecting
// its name.
func validInstrumentName(name string) bool {
	if name == "" || len(name) > 255 || !isLetter(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if c := name[i]; !isLetter(c) && (c < '0' || c > '9') && !strings.ContainsRune("_.-/", rune(c)) {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

type instruments struct {
	mu         sync.RWMutex
	limit      int
	counters   map[string]metric.Float64Counter
	gauges     map[string]metric.Float64Gauge
	histograms map[string]metric.Float64Histogram
}

func newInstruments(limit int) *instruments {
	return &instruments{
		limit:      limit,
		counters:   map[string]metric.Float64Counter{},
		gauges:     map[string]metric.Float64Gauge{},
		histograms: map[string]metric.Float64Histogram{},
	}
}

func (s *instruments) full() bool {
	return s.limit > 0 && len(s.counters)+len(s.gauges)+len(s.histograms) >= s.limit
}

func lookup[I any](s *instruments, byName map[string]I, name string, create func() (I, error)) (I, error) {
	s.mu.RLock()
	inst, ok := byName[name]
	full := s.full()
	s.mu.RUnlock()
	if ok {
		return inst, nil
	}
	if !validInstrumentName(name) {
		return inst, errInvalidName
	}
	if full {
		return inst, errNamesFull
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := byName[name]; ok {
		return inst, nil
	}
	if s.full() {
		return inst, errNamesFull
	}
	inst, err := create()
	if err != nil {
		return inst, err
	}
	byName[name] = inst
	return inst, nil
}

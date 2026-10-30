// Command quicmetrics receives DogStatsD counters, gauges and histograms over raw QUIC and
// exposes them on a Prometheus /metrics endpoint.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/r0mdau/quicmetrics/internal/protocol"
	"github.com/r0mdau/quicmetrics/internal/server"
)

const (
	shutdownTimeout          = 5 * time.Second
	defaultTLSReloadInterval = 30 * time.Second
)

var errUsage = errors.New("invalid command line")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "quicmetrics:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("quicmetrics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", protocol.DefaultAddr, "UDP address the QUIC listener binds to")
	metricsAddr := fs.String("metrics-addr", ":2223", "TCP address of the Prometheus /metrics endpoint")
	idleTimeout := fs.Duration("idle-timeout", server.DefaultIdleTimeout, "close a session after this long without traffic")
	maxSessions := fs.Int("max-sessions", server.DefaultMaxSessions, "maximum number of concurrent QUIC sessions, others are rejected")
	timeouts := server.DefaultTimeouts()
	fs.DurationVar(&timeouts.FirstStream, "first-stream-timeout", timeouts.FirstStream, "close a session that sends no data on its stream this long after the handshake, 0 for no deadline")
	fs.DurationVar(&timeouts.Line, "line-timeout", timeouts.Line, "close a session that sends no complete line for this long, 0 for no deadline")
	fs.DurationVar(&timeouts.Ack, "ack-timeout", timeouts.Ack, "close a session that does not take an ack within this long, 0 for no deadline")
	tlsCert := fs.String("tls-cert", "", "PEM certificate chain of the QUIC listener, reloaded when the file changes (with -tls-key)")
	tlsKey := fs.String("tls-key", "", "PEM private key of the QUIC listener (with -tls-cert)")
	tlsClientCA := fs.String("tls-client-ca", "", "PEM bundle of the CAs that sign client certificates, reloaded when the file changes: when set, clients must present a valid certificate (mTLS)")
	tlsReload := fs.Duration("tls-reload-interval", defaultTLSReloadInterval, "how often -tls-cert, -tls-key and -tls-client-ca are checked for changes")
	limits := server.DefaultLimits()
	fs.IntVar(&limits.MaxNames, "max-metric-names", limits.MaxNames, "maximum number of counters, gauges and histograms by name, 0 for no limit")
	fs.IntVar(&limits.MaxTags, "max-tags", limits.MaxTags, "maximum number of tags per metric, 0 for no limit")
	fs.IntVar(&limits.MaxTagKeyLen, "max-tag-key-len", limits.MaxTagKeyLen, "maximum length of a tag key in bytes, 0 for no limit")
	fs.IntVar(&limits.MaxTagValueLen, "max-tag-value-len", limits.MaxTagValueLen, "maximum length of a tag value in bytes, 0 for no limit")
	seriesTTL := fs.Duration("series-ttl", server.DefaultSeriesTTL, "forget a series no metric updated for this long, 0 to keep every series")
	var level slog.Level
	fs.TextVar(&level, "log-level", slog.LevelInfo, "minimum log level: debug, info, warn or error")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *showVersion {
		fmt.Fprintln(stderr, "quicmetrics", version())
		return nil
	}
	var invalid error
	switch {
	case (*tlsCert == "") != (*tlsKey == ""):
		invalid = errors.New("-tls-cert and -tls-key must be set together")
	case *tlsReload <= 0:
		invalid = fmt.Errorf("-tls-reload-interval must be positive, got %v", *tlsReload)
	case min(limits.MaxNames, limits.MaxTags, limits.MaxTagKeyLen, limits.MaxTagValueLen) < 0:
		invalid = fmt.Errorf("-max-metric-names, -max-tags, -max-tag-key-len and -max-tag-value-len must not be negative, got %+v", limits)
	case min(timeouts.FirstStream, timeouts.Line, timeouts.Ack) < 0:
		invalid = fmt.Errorf("-first-stream-timeout, -line-timeout and -ack-timeout must not be negative, got %+v", timeouts)
	case *seriesTTL < 0:
		invalid = fmt.Errorf("-series-ttl must not be negative, got %v", *seriesTTL)
	}
	if invalid != nil {
		fmt.Fprintln(stderr, "quicmetrics:", invalid)
		return fmt.Errorf("%w: %w", errUsage, invalid)
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	if *maxSessions < 1 {
		return fmt.Errorf("-max-sessions must be at least 1, got %d", *maxSessions)
	}
	quicConf, err := server.QUICConfig(*idleTimeout)
	if err != nil {
		return fmt.Errorf("-idle-timeout: %w", err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	provider, err := server.NewPrometheusMeterProvider(registry, server.WithSeriesTTL(*seriesTTL))
	if err != nil {
		return err
	}
	meter := provider.Meter("github.com/r0mdau/quicmetrics")

	var (
		tlsConf *tls.Config
		certs   *server.CertReloader
	)
	if *tlsCert != "" {
		certs, err = server.NewCertReloader(*tlsCert, *tlsKey, logger)
		if err != nil {
			return fmt.Errorf("create TLS config: %w", err)
		}
		tlsConf = certs.TLSConfig()
	} else {
		logger.Warn("no -tls-cert and -tls-key: using a self-signed certificate generated at start, clients cannot verify it, for development only")
		tlsConf, err = server.SelfSignedTLSConfig()
		if err != nil {
			return fmt.Errorf("create TLS config: %w", err)
		}
	}
	var clientCAs *server.CAReloader
	if *tlsClientCA != "" {
		clientCAs, err = server.NewCAReloader(*tlsClientCA, logger)
		if err != nil {
			return fmt.Errorf("create TLS config: %w", err)
		}
		clientCAs.RequireClientCerts(tlsConf)
	}
	listener, err := server.Listen(*addr, tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("listen QUIC on %s: %w", *addr, err)
	}
	defer func() { _ = listener.Release() }()

	httpListener, err := net.Listen("tcp", *metricsAddr)
	if err != nil {
		return fmt.Errorf("listen HTTP on %s: %w", *metricsAddr, err)
	}
	errorLog := slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.InstrumentMetricHandler(registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog:      errorLog,
		ErrorHandling: promhttp.ContinueOnError,
	})))
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: errorLog}

	srv := server.New(server.NewAggregator(meter, logger, server.WithLimits(limits)), logger, server.WithMaxSessions(*maxSessions), server.WithTimeouts(timeouts))

	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var wg sync.WaitGroup
	wg.Go(func() {
		logger.Info("serving QUIC metrics listener", "addr", listener.Addr().String(), "version", version())
		if err := srv.Serve(ctx, listener.Listener); err != nil {
			cancel(err)
		}
	})
	if certs != nil {
		wg.Go(func() { certs.Watch(ctx, *tlsReload) })
	}
	if clientCAs != nil {
		wg.Go(func() { clientCAs.Watch(ctx, *tlsReload) })
	}
	wg.Go(func() {
		logger.Info("serving Prometheus metrics", "addr", httpListener.Addr().String(), "path", "/metrics")
		if err := httpServer.Serve(httpListener); !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("serve metrics: %w", err))
		}
	})

	<-ctx.Done()
	var runErr error
	if parent.Err() == nil {
		runErr = context.Cause(ctx)
	}
	logger.Info("shutting down")

	shutdownCtx, stopShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer stopShutdown()
	_ = listener.Close()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shut down metrics server: %w", err))
	}
	wg.Wait()
	if err := provider.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shut down meter provider: %w", err))
	}
	return runErr
}

// version is the module version the go command stamped from version control, such as
// v0.0.0-20261027023032-95a7d97a2a33 for an untagged commit, or (devel) without it.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return info.Main.Version
}

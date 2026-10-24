// Command quicmetrics receives DogStatsD counters and gauges over raw QUIC and exposes them
// on a Prometheus /metrics endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
	"github.com/r0mdau/quicmetrics/internal/server"
)

const shutdownTimeout = 5 * time.Second

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
	var level slog.Level
	fs.TextVar(&level, "log-level", slog.LevelInfo, "minimum log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	provider, err := server.NewPrometheusMeterProvider(registry)
	if err != nil {
		return err
	}
	meter := provider.Meter("github.com/r0mdau/quicmetrics")

	tlsConf, err := server.SelfSignedTLSConfig()
	if err != nil {
		return fmt.Errorf("create TLS config: %w", err)
	}
	listener, err := quic.ListenAddr(*addr, tlsConf, nil)
	if err != nil {
		return fmt.Errorf("listen QUIC on %s: %w", *addr, err)
	}
	defer func() { _ = listener.Close() }()

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

	srv := server.New(server.NewAggregator(meter, logger), logger)

	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var wg sync.WaitGroup
	wg.Go(func() {
		logger.Info("serving QUIC metrics listener", "addr", listener.Addr().String())
		if err := srv.Serve(ctx, listener); err != nil {
			cancel(err)
		}
	})
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

// Command quicmetrics-client generates realistic DogStatsD counters and gauges, sends them to a
// quicmetrics server and waits for each one to be acknowledged.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

const (
	minRate = 0.001
	maxRate = 1e9
)

var errUsage = errors.New("invalid command line")

// options controls how many metrics are sent and how fast. An interval of 0 sends as fast
// as acks come back.
type options struct {
	interval time.Duration
	count    int
	duration time.Duration
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "quicmetrics-client:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("quicmetrics-client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", protocol.DefaultAddr, "UDP address of the quicmetrics server")
	rate := fs.Float64("rate", 10, fmt.Sprintf("metrics sent per second (%g to %g), 0 sends as fast as acks come back", minRate, maxRate))
	var opts options
	fs.IntVar(&opts.count, "count", 0, "stop after this many metrics, 0 means no limit")
	fs.DurationVar(&opts.duration, "duration", 10*time.Second, "stop this long after connecting, 0 runs until interrupted")
	functions := fs.Int("functions", 5, fmt.Sprintf("number of simulated functions (1-%d), drives tag cardinality", len(functionNames)))
	seed := fs.Uint64("seed", 0, "random seed to replay a run, 0 picks one")
	var level slog.Level
	fs.TextVar(&level, "log-level", slog.LevelInfo, "minimum log level: debug also logs every acknowledged metric")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	var invalid error
	switch {
	case opts.count < 0 || opts.duration < 0:
		invalid = errors.New("-count and -duration must not be negative")
	case *rate != 0 && !(*rate >= minRate && *rate <= maxRate):
		invalid = fmt.Errorf("-rate must be 0 or between %g and %g, got %g", minRate, maxRate, *rate)
	case *functions < 1 || *functions > len(functionNames):
		invalid = fmt.Errorf("-functions must be between 1 and %d, got %d", len(functionNames), *functions)
	}
	if invalid != nil {
		fmt.Fprintln(stderr, "quicmetrics-client:", invalid)
		return fmt.Errorf("%w: %w", errUsage, invalid)
	}
	if *rate != 0 {
		opts.interval = time.Duration(float64(time.Second) / *rate)
	}
	if *seed == 0 {
		*seed = rand.Uint64() //nolint:gosec // simulated traffic, not security sensitive
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	logger.Info("generating metrics", "addr", *addr, "seed", *seed, "rate", *rate,
		"count", opts.count, "duration", opts.duration, "functions", *functions)
	if _, err := sendMetrics(ctx, logger, *addr, newGenerator(*seed, *functions), opts); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// sendMetrics sends generated metrics until opts.count, opts.duration or ctx stops it, and
// returns how many were acknowledged. Stopping once connected is not an error.
func sendMetrics(ctx context.Context, logger *slog.Logger, addr string, gen *generator, opts options) (sent int, err error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the server uses a throwaway self-signed certificate
		NextProtos:         []string{protocol.ALPN},
		MinVersion:         tls.VersionTLS13,
	}
	conn, err := quic.DialAddr(ctx, addr, tlsConf, nil)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.CloseWithError(0, "done") }()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return 0, fmt.Errorf("open stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	if opts.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.duration)
		defer cancel()
	}
	stopAfterFunc := context.AfterFunc(ctx, func() { _ = conn.CloseWithError(0, "stopped") })
	defer stopAfterFunc()

	start := time.Now()
	defer func() {
		logger.Info("metrics acknowledged", "count", sent, "elapsed", time.Since(start).Round(time.Millisecond))
	}()
	acks := bufio.NewReader(stream)
	timer := time.NewTimer(0)
	defer timer.Stop()
	next := start
	for opts.count == 0 || sent < opts.count {
		select {
		case <-ctx.Done():
			return sent, nil
		case <-timer.C:
		}
		m := gen.next()
		if err := send(stream, acks, m); err != nil {
			if ctx.Err() != nil {
				return sent, nil
			}
			return sent, err
		}
		sent++
		logger.Debug("metric acknowledged", "metric", m)
		next = next.Add(opts.interval)
		if now := time.Now(); next.Before(now) {
			next = now
		}
		timer.Reset(time.Until(next))
	}
	return sent, nil
}

func send(stream *quic.Stream, acks *bufio.Reader, metric string) error {
	if _, err := stream.Write([]byte(metric + "\n")); err != nil {
		return fmt.Errorf("send %q: %w", metric, err)
	}
	ack, err := acks.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read ack for %q: %w", metric, err)
	}
	if ack != metric+"\n" {
		return fmt.Errorf("ack %q does not match sent metric %q", ack, metric)
	}
	return nil
}

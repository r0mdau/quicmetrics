// Command quicmetrics-client generates realistic DogStatsD counters, gauges and histograms,
// sends them to a quicmetrics server in pipelined batches and waits for them to be acknowledged.
package main

import (
	"bufio"
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

const (
	minRate = 0.001
	maxRate = 1e9

	defaultBatch  = 100
	maxBatch      = 10000
	windowBatches = 4
	drainTimeout  = 5 * time.Second
)

var errUsage = errors.New("invalid command line")

// options controls how many metrics are sent and how fast. An interval of 0 sends as fast
// as acks come back. A batch below 1 means 1.
type options struct {
	interval time.Duration
	count    int
	duration time.Duration
	batch    int
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
	fs.IntVar(&opts.batch, "batch", defaultBatch, fmt.Sprintf("maximum metrics per write (1-%d), lines due together share a write", maxBatch))
	functions := fs.Int("functions", 5, fmt.Sprintf("number of simulated functions (1-%d), drives tag cardinality", len(functionNames)))
	seed := fs.Uint64("seed", 0, "random seed to replay a run, 0 picks one")
	var level slog.Level
	fs.TextVar(&level, "log-level", slog.LevelInfo, "minimum log level: debug also logs every metric sent")
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
	case opts.batch < 1 || opts.batch > maxBatch:
		invalid = fmt.Errorf("-batch must be between 1 and %d, got %d", maxBatch, opts.batch)
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
		"count", opts.count, "duration", opts.duration, "batch", opts.batch, "functions", *functions)
	if _, err := sendMetrics(ctx, logger, *addr, newGenerator(*seed, *functions), opts); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// sendMetrics sends generated metrics until opts.count, opts.duration or ctx stops it, and
// returns how many were acknowledged. Stopping once connected is not an error.
func sendMetrics(ctx context.Context, logger *slog.Logger, addr string, gen *generator, opts options) (acked int, err error) {
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

	sendCtx := ctx
	if opts.duration > 0 {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, opts.duration)
		defer cancel()
		deadline, _ := sendCtx.Deadline()
		_ = stream.SetWriteDeadline(deadline)
	}
	stopAfterFunc := context.AfterFunc(ctx, func() { _ = conn.CloseWithError(0, "stopped") })
	defer stopAfterFunc()

	acks := make(chan ackResult)
	quit := make(chan struct{})
	var reader sync.WaitGroup
	reader.Go(func() { readAcks(stream, acks, quit) })
	defer func() {
		close(quit)
		_ = conn.CloseWithError(0, "done")
		reader.Wait()
	}()

	start := time.Now()
	p := &pipeline{logger: logger, stream: stream, gen: gen, opts: opts}
	p.opts.batch = max(opts.batch, 1)
	defer func() {
		acked = p.acked
		logger.Info("metrics acknowledged", "count", acked, "elapsed", time.Since(start).Round(time.Millisecond))
	}()
	if err := p.run(ctx, sendCtx, acks); err != nil && ctx.Err() == nil {
		return p.acked, err
	}
	return p.acked, nil
}

type ackResult struct {
	processed int
	err       error
}

func readAcks(stream io.Reader, acks chan<- ackResult, quit <-chan struct{}) {
	r := bufio.NewReader(stream)
	for {
		var res ackResult
		line, err := r.ReadString('\n')
		if err != nil {
			res.err = fmt.Errorf("read ack: %w", err)
		} else if res.processed, err = strconv.Atoi(strings.TrimSuffix(line, "\n")); err != nil {
			res.err = fmt.Errorf("invalid ack %q: %w", line, err)
		}
		select {
		case acks <- res:
		case <-quit:
			return
		}
		if res.err != nil {
			return
		}
	}
}

type pipeline struct {
	logger *slog.Logger
	stream *quic.Stream
	gen    *generator
	opts   options
	sent   int
	acked  int
}

// run stops sending when sendCtx is done but still drains acks, unless its parent ctx is done.
func (p *pipeline) run(ctx, sendCtx context.Context, acks <-chan ackResult) error {
	window := p.opts.batch * windowBatches
	timer := time.NewTimer(0)
	defer timer.Stop()
	next := time.Now()
	debug := p.logger.Enabled(ctx, slog.LevelDebug)
	var buf []byte
	for p.opts.count == 0 || p.sent < p.opts.count {
		due := timer.C
		if p.sent-p.acked >= window {
			due = nil
		}
		select {
		case <-sendCtx.Done():
			return p.drain(ctx, acks)
		case res := <-acks:
			if err := p.ack(res); err != nil {
				return err
			}
		case now := <-due:
			n := p.opts.batch
			if p.opts.interval > 0 {
				n = min(n, max(1, 1+int(now.Sub(next)/p.opts.interval)))
			}
			if p.opts.count > 0 {
				n = min(n, p.opts.count-p.sent)
			}
			n = min(n, window-(p.sent-p.acked))
			buf = buf[:0]
			for range n {
				m := p.gen.next()
				if debug {
					p.logger.Debug("metric sent", "metric", m)
				}
				buf = append(append(buf, m...), '\n')
			}
			if w, err := p.stream.Write(buf); err != nil {
				written := bytes.Count(buf[:w], []byte{'\n'})
				p.sent += written
				if sendCtx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
					return p.drain(ctx, acks)
				}
				return fmt.Errorf("send %d metrics: %w", n, err)
			}
			p.sent += n
			next = next.Add(time.Duration(n) * p.opts.interval)
			if now := time.Now(); next.Before(now) {
				next = now
			}
			timer.Reset(time.Until(next))
		}
	}
	return p.drain(ctx, acks)
}

func (p *pipeline) drain(ctx context.Context, acks <-chan ackResult) error {
	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()
	for p.acked < p.sent {
		select {
		case <-drainCtx.Done():
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("%d of %d metrics not acknowledged after %v", p.sent-p.acked, p.sent, drainTimeout)
		case res := <-acks:
			if err := p.ack(res); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *pipeline) ack(res ackResult) error {
	if res.err != nil {
		return res.err
	}
	if res.processed <= p.acked || res.processed > p.sent {
		return fmt.Errorf("ack %d out of range: %d metrics acknowledged, %d sent", res.processed, p.acked, p.sent)
	}
	p.acked = res.processed
	return nil
}

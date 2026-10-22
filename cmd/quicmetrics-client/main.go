// Command quicmetrics-client sends a few sample DogStatsD counters to a quicmetrics server
// and waits for each one to be acknowledged.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

var sampleMetrics = []string{
	"users.online:1|c|#country:china,city:beijing",
	"users.online:2|c|#country:usa,city:losangeles",
	"users.online:3|c|#country:usa",
}

func main() {
	addr := flag.String("addr", protocol.DefaultAddr, "UDP address of the quicmetrics server")
	flag.Parse()

	if err := run(context.Background(), slog.Default(), *addr, sampleMetrics); err != nil {
		fmt.Fprintln(os.Stderr, "quicmetrics-client:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, addr string, metrics []string) error {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the server uses a throwaway self-signed certificate
		NextProtos:         []string{protocol.ALPN},
		MinVersion:         tls.VersionTLS13,
	}
	conn, err := quic.DialAddr(ctx, addr, tlsConf, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.CloseWithError(0, "done") }()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	for _, m := range metrics {
		if err := send(stream, m); err != nil {
			return err
		}
		logger.Info("metric acknowledged", "metric", m)
	}
	return nil
}

func send(stream *quic.Stream, metric string) error {
	if _, err := stream.Write([]byte(metric)); err != nil {
		return fmt.Errorf("send %q: %w", metric, err)
	}
	ack := make([]byte, len(metric))
	if _, err := io.ReadFull(stream, ack); err != nil {
		return fmt.Errorf("read ack for %q: %w", metric, err)
	}
	if string(ack) != metric {
		return fmt.Errorf("ack %q does not match sent metric %q", ack, metric)
	}
	return nil
}

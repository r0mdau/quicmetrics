// Package server receives DogStatsD metrics over raw QUIC and aggregates them with the
// OpenTelemetry metrics SDK.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/quic-go/quic-go"
)

// Server accepts QUIC connections and feeds every received metric to an Aggregator.
//
// Each connection carries one stream. Every Write on that stream is one metric, and it is
// echoed back to the client as an acknowledgement.
type Server struct {
	agg      *Aggregator
	logger   *slog.Logger
	sessions sync.WaitGroup
}

// New returns a Server that records metrics with agg and logs to logger.
func New(agg *Aggregator, logger *slog.Logger) *Server {
	return &Server{agg: agg, logger: logger}
}

// Serve accepts connections on listener until ctx is done or the listener is closed, then
// waits for open sessions to end. Cancelling ctx also closes open sessions. Serve returns
// nil in both cases and only returns an error when Accept fails for another reason.
// Serve must not be called concurrently on the same Server.
func (s *Server) Serve(ctx context.Context, listener *quic.Listener) error {
	defer s.sessions.Wait()
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, quic.ErrServerClosed) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept QUIC connection: %w", err)
		}
		s.sessions.Go(func() { s.handleConn(ctx, conn) })
	}
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn) {
	logger := s.logger.With("remote", conn.RemoteAddr().String())
	logger.Info("session accepted")
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseWithError(0, "server shutting down") })
	defer func() {
		stop()
		if r := recover(); r != nil {
			logger.Error("panic in session", "panic", r, "stack", string(debug.Stack()))
		}
		_ = conn.CloseWithError(0, "bye")
		logger.Info("session closed")
	}()

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		if !isExpectedClose(ctx, err) {
			logger.Warn("accept stream", "err", err)
		}
		return
	}
	w := echoWriter{ctx: context.WithoutCancel(ctx), w: stream, agg: s.agg}
	if _, err := io.Copy(w, stream); err != nil && !isExpectedClose(ctx, err) {
		logger.Warn("session ended with error", "err", err)
	}
}

func isExpectedClose(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var appErr *quic.ApplicationError
	return errors.As(err, &appErr) && appErr.Remote && appErr.ErrorCode == 0
}

// echoWriter records every Write as one metric and echoes it back as an acknowledgement.
type echoWriter struct {
	ctx context.Context
	w   io.Writer
	agg *Aggregator
}

func (e echoWriter) Write(b []byte) (int, error) {
	e.agg.Record(e.ctx, string(b))
	return e.w.Write(b)
}

// Package server receives DogStatsD metrics over raw QUIC and aggregates them with the
// OpenTelemetry metrics SDK.
package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

// MaxLineLength is the longest line in bytes, without its terminator, that a session accepts.
const MaxLineLength = 8 << 10

// DefaultMaxSessions is the session cap of a Server created without WithMaxSessions.
const DefaultMaxSessions = 1000

var errLineTooLong = fmt.Errorf("line longer than %d bytes", MaxLineLength)

// Server accepts QUIC connections and feeds every received metric to an Aggregator.
//
// Each connection carries one stream of newline-delimited metrics, acknowledged in bulk.
//
// A Server runs a bounded number of sessions at once.
type Server struct {
	agg      *Aggregator
	logger   *slog.Logger
	sessions sync.WaitGroup
	slots    chan struct{}
}

// Option customizes a Server.
type Option func(*Server)

// WithMaxSessions caps concurrent sessions at n; further connections are closed with
// protocol.CodeServerBusy.
func WithMaxSessions(n int) Option {
	return func(s *Server) { s.slots = make(chan struct{}, n) }
}

// New returns a Server that records metrics with agg and logs to logger.
func New(agg *Aggregator, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{agg: agg, logger: logger}
	WithMaxSessions(DefaultMaxSessions)(s)
	for _, opt := range opts {
		opt(s)
	}
	return s
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
		select {
		case s.slots <- struct{}{}:
			s.sessions.Go(func() {
				defer func() { <-s.slots }()
				s.handleConn(ctx, conn)
			})
		default:
			s.logger.Warn("too many sessions, connection rejected", "remote", conn.RemoteAddr().String(), "max", cap(s.slots))
			_ = conn.CloseWithError(protocol.CodeServerBusy, "too many sessions")
		}
	}
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn) {
	logger := s.logger.With("remote", conn.RemoteAddr().String())
	if certs := conn.ConnectionState().TLS.PeerCertificates; len(certs) > 0 {
		logger = logger.With("client_subject", certs[0].Subject.String())
	}
	logger.Info("session accepted")
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseWithError(protocol.CodeNoError, "server shutting down") })
	closeCode, closeReason := quic.ApplicationErrorCode(protocol.CodeNoError), "bye"
	defer func() {
		stop()
		if r := recover(); r != nil {
			logger.Error("panic in session", "panic", r, "stack", string(debug.Stack()))
		}
		_ = conn.CloseWithError(closeCode, closeReason)
		logger.Info("session closed")
	}()

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		if !isExpectedClose(ctx, err) {
			logger.Warn("accept stream", "err", err)
		}
		return
	}
	err = s.serveStream(context.WithoutCancel(ctx), stream)
	switch {
	case errors.Is(err, errLineTooLong):
		closeCode, closeReason = protocol.CodeLineTooLong, "line too long"
		logger.Debug("closing session", "err", err)
		s.agg.lineTooLong(ctx, clientName(conn))
	case err != nil && !isExpectedClose(ctx, err):
		logger.Warn("session ended with error", "err", err)
	}
}

func clientName(conn *quic.Conn) string {
	name := conn.RemoteAddr().String()
	if certs := conn.ConnectionState().TLS.PeerCertificates; len(certs) > 0 {
		name += " " + certs[0].Subject.String()
	}
	return name
}

func isExpectedClose(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if _, ok := errors.AsType[*quic.IdleTimeoutError](err); ok {
		return true
	}
	appErr, ok := errors.AsType[*quic.ApplicationError](err)
	return ok && appErr.Remote && appErr.ErrorCode == protocol.CodeNoError
}

func (s *Server) serveStream(ctx context.Context, rw io.ReadWriter) error {
	r := bufio.NewReaderSize(rw, MaxLineLength+2)
	var processed, acked uint64
	var ack []byte
	flush := func() error {
		if processed == acked {
			return nil
		}
		ack = append(strconv.AppendUint(ack[:0], processed, 10), '\n')
		if _, err := rw.Write(ack); err != nil {
			return fmt.Errorf("write ack: %w", err)
		}
		acked = processed
		return nil
	}
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return errLineTooLong
		}
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return fmt.Errorf("read metric: %w", err)
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) > MaxLineLength {
			return errLineTooLong
		}
		if len(line) > 0 {
			s.agg.Record(ctx, string(line))
			processed++
		}
		if eof || !hasBufferedLine(r) {
			if err := flush(); err != nil {
				return err
			}
		}
		if eof {
			return nil
		}
	}
}

func hasBufferedLine(r *bufio.Reader) bool {
	buffered, _ := r.Peek(r.Buffered())
	return bytes.IndexByte(buffered, '\n') >= 0
}

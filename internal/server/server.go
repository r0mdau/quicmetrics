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
)

// MaxLineLength is the longest line in bytes, without its terminator, that a session accepts.
const MaxLineLength = 8 << 10

const errCodeLineTooLong quic.ApplicationErrorCode = 1

var errLineTooLong = fmt.Errorf("line longer than %d bytes", MaxLineLength)

// Server accepts QUIC connections and feeds every received metric to an Aggregator.
//
// Each connection carries one stream of newline-delimited metrics, acknowledged in bulk.
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
	closeCode, closeReason := quic.ApplicationErrorCode(0), "bye"
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
		logger.Warn("closing session", "err", err)
		closeCode, closeReason = errCodeLineTooLong, "line too long"
	case err != nil && !isExpectedClose(ctx, err):
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

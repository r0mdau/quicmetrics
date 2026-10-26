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
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/r0mdau/quicmetrics/internal/protocol"
)

// MaxLineLength is the longest line in bytes, without its terminator, that a session accepts.
const MaxLineLength = 8 << 10

// DefaultMaxSessions is the session cap of a Server created without WithMaxSessions.
const DefaultMaxSessions = 1000

var (
	errLineTooLong = fmt.Errorf("line longer than %d bytes", MaxLineLength)

	errNoStream   = errors.New("no stream opened")
	errNoLine     = errors.New("no line received")
	errAckNotRead = errors.New("ack not read")
)

// Timeouts bounds how long a session may hold a slot without making progress, however many
// packets it sends. A timeout of 0 or less disables its deadline.
type Timeouts struct {
	// FirstStream is how long a client has to send its first bytes on its stream after the handshake.
	FirstStream time.Duration
	// Line is how long a session may go without sending a complete non-empty line.
	Line time.Duration
	// Ack is how long the server waits for the client to take an ack.
	Ack time.Duration
}

// DefaultTimeouts returns the timeouts of a Server created without WithTimeouts.
func DefaultTimeouts() Timeouts {
	return Timeouts{FirstStream: 10 * time.Second, Line: time.Minute, Ack: 10 * time.Second}
}

// deadlineStream is the stream of a session: a quic.Stream or a net.Conn.
type deadlineStream interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// Server accepts QUIC connections and feeds every received metric to an Aggregator.
//
// Each connection carries one stream of newline-delimited metrics, acknowledged in bulk.
//
// A Server runs a bounded number of sessions at once, and closes those that stop making progress.
type Server struct {
	agg      *Aggregator
	logger   *slog.Logger
	timeouts Timeouts
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

// WithTimeouts sets the deadlines of a session, closed with protocol.CodeSlowClient when missed.
func WithTimeouts(t Timeouts) Option {
	return func(s *Server) { s.timeouts = t }
}

// New returns a Server that records metrics with agg and logs to logger.
func New(agg *Aggregator, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{agg: agg, logger: logger, timeouts: DefaultTimeouts()}
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

	stream, err := s.acceptStream(ctx, conn)
	if err == nil {
		err = s.serveStream(context.WithoutCancel(ctx), stream)
	}
	switch {
	case errors.Is(err, errLineTooLong):
		closeCode, closeReason = protocol.CodeLineTooLong, "line too long"
		logger.Debug("closing session", "err", err)
		s.agg.lineTooLong(ctx, clientName(conn))
	case errors.Is(err, errNoStream), errors.Is(err, errNoLine), errors.Is(err, errAckNotRead):
		closeCode, closeReason = protocol.CodeSlowClient, err.Error()
		logger.Debug("closing session", "err", err)
		s.agg.slowClient(ctx, clientName(conn))
	case err != nil && !isExpectedClose(ctx, err):
		logger.Warn("session ended with error", "err", err)
	}
}

func (s *Server) acceptStream(ctx context.Context, conn *quic.Conn) (*quic.Stream, error) {
	acceptCtx := ctx
	if s.timeouts.FirstStream > 0 {
		var cancel context.CancelFunc
		acceptCtx, cancel = context.WithTimeout(ctx, s.timeouts.FirstStream)
		defer cancel()
	}
	stream, err := conn.AcceptStream(acceptCtx)
	switch {
	case err == nil:
		return stream, nil
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return nil, errNoStream
	default:
		return nil, fmt.Errorf("accept stream: %w", err)
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

func (s *Server) serveStream(ctx context.Context, rw deadlineStream) error {
	r := bufio.NewReaderSize(rw, MaxLineLength+2)
	var processed, acked uint64
	var ack []byte
	flush := func() error {
		if processed == acked {
			return nil
		}
		ack = append(strconv.AppendUint(ack[:0], processed, 10), '\n')
		if err := rw.SetWriteDeadline(deadline(s.timeouts.Ack)); err != nil {
			return fmt.Errorf("set ack deadline: %w", err)
		}
		if _, err := rw.Write(ack); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return errAckNotRead
			}
			return fmt.Errorf("write ack: %w", err)
		}
		acked = processed
		if err := rw.SetReadDeadline(deadline(s.timeouts.Line)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
		return nil
	}
	if err := rw.SetReadDeadline(deadline(s.timeouts.Line)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return errLineTooLong
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return errNoLine
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

func deadline(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}

func hasBufferedLine(r *bufio.Reader) bool {
	buffered, _ := r.Peek(r.Buffered())
	return bytes.IndexByte(buffered, '\n') >= 0
}

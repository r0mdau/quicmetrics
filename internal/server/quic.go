package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	// DefaultIdleTimeout is how long a session may stay silent before the server drops it.
	DefaultIdleTimeout = 30 * time.Second

	handshakeIdleTimeout = 5 * time.Second

	initialStreamWindow     = 64 << 10
	maxStreamWindow         = 256 << 10
	initialConnectionWindow = 128 << 10
	maxConnectionWindow     = 512 << 10
)

// QUICConfig returns the listener config: one bidirectional stream, no unidirectional stream
// and small flow-control windows.
func QUICConfig(idleTimeout time.Duration) (*quic.Config, error) {
	if idleTimeout <= 0 {
		return nil, fmt.Errorf("idle timeout must be positive, got %s", idleTimeout)
	}
	return &quic.Config{
		HandshakeIdleTimeout:           handshakeIdleTimeout,
		MaxIdleTimeout:                 idleTimeout,
		MaxIncomingStreams:             1,
		MaxIncomingUniStreams:          -1,
		InitialStreamReceiveWindow:     initialStreamWindow,
		MaxStreamReceiveWindow:         maxStreamWindow,
		InitialConnectionReceiveWindow: initialConnectionWindow,
		MaxConnectionReceiveWindow:     maxConnectionWindow,
	}, nil
}

// Listener is a quic.Listener that also owns the UDP socket and transport behind it.
type Listener struct {
	*quic.Listener
	transport *quic.Transport
	conn      net.PacketConn
}

// Listen binds addr and requires a Retry from every client. The caller must call Release.
func Listen(addr string, tlsConf *tls.Config, conf *quic.Config) (*Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	transport := &quic.Transport{Conn: conn, VerifySourceAddress: verifySourceAddress}
	listener, err := transport.Listen(tlsConf, conf)
	if err != nil {
		_ = transport.Close()
		_ = conn.Close()
		return nil, err
	}
	return &Listener{Listener: listener, transport: transport, conn: conn}, nil
}

// Release closes the listener, then abruptly the open connections and the UDP socket.
func (l *Listener) Release() error {
	return errors.Join(l.Close(), l.transport.Close(), l.conn.Close())
}

// verifySourceAddress returns true to require a Retry.
func verifySourceAddress(net.Addr) bool { return true }

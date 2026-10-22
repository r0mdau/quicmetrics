// Package protocol holds the wire-level settings shared by the quicmetrics server and client.
package protocol

const (
	// ALPN is the TLS application protocol that client and server must both negotiate.
	ALPN = "quic-echo-example"

	// DefaultAddr is the default UDP address of the QUIC metrics listener.
	DefaultAddr = "localhost:4242"
)

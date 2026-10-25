package protocol

// QUIC application error codes the server closes a connection with, unique across the protocol.
const (
	CodeNoError     = 0
	CodeLineTooLong = 1
	CodeServerBusy  = 2
)

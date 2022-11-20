# quicmetrics

Receive and aggregate fleeting metrics (from FaaS).

This proof of concept uses:
- [QUIC](https://peering.google.com/#/learn-more/quic) without http3 as a transport protocol
- statsd string format for metrics

In the `client/main.go` example, we send the multiple metric counter, to confirm it is summed in prometheus endpoint output.

Library used:
- [quic-go](https://github.com/lucas-clemente/quic-go), a QUIC implementation in pure go
- [go-dogstatsd-parser](https://github.com/narqo/go-dogstatsd-parser), a standalone parser for DogStatsD metrics protocol

## Getting started

Launch the server

    go run main.go


Send sample metrics

    go run client/main.go


View prometheus metrics

    curl localhost:2223/metrics

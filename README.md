# quicmetrics

Receive and aggregate fleeting metrics (from FaaS).

This proof of concept uses:
- [QUIC](https://peering.google.com/#/learn-more/quic) without http3 as a transport protocol
- statsd string format for metrics

In the `client/main.go` example, we send multiple metric counter, to confirm it is summed in the prometheus endpoint output.

Library used:
- [quic-go](https://github.com/quic-go/quic-go), a QUIC implementation in pure go
- [go-dogstatsd-parser](https://github.com/narqo/go-dogstatsd-parser), a standalone parser for DogStatsD metrics protocol
- [prometheus/client_golang](github.com/prometheus/client_golang), the Go client library for prometheus
- [opentelemetry-go](https://github.com/open-telemetry/opentelemetry-go), the Go implementation of OpenTelemetry

## Getting started

Launch the server

    go run main.go


Send sample metrics

    go run client/main.go


View prometheus metrics

    curl localhost:2223/metrics

## Run with docker compose

The [compose.yaml](compose.yaml) suite runs the server together with a
[Prometheus](https://prometheus.io) instance that scrapes it every 5 seconds
(see [prometheus/prometheus.yml](prometheus/prometheus.yml)).

| Service | Port on the host | Purpose |
| --- | --- | --- |
| `quicmetrics` | `4242/udp` | QUIC listener receiving metrics |
| `quicmetrics` | `2223/tcp` | Prometheus `/metrics` endpoint |
| `prometheus` | `9090/tcp` | Prometheus UI |

Build the binary and start the suite (the image packages the prebuilt
`build/quicmetrics` binary, so `make build` must run first):

    make compose-up

which is equivalent to:

    make build
    docker compose up --build -d

Send sample metrics from your machine to the server running in docker compose:

    go run client/main.go

Check the metrics:

- raw endpoint: `curl localhost:2223/metrics`
- Prometheus UI: open
  <http://localhost:9090/query?g0.expr=%7B__name__%3D~%22users_online.*%22%7D>
  (query `{__name__=~"users_online.*"}`) and click **Execute**. Switch to the
  **Graph** tab to watch the counters grow each time you rerun the client.
  The regex is needed because the current OpenTelemetry Prometheus exporter
  appends `_total` once per attribute set (`users_online_total`,
  `users_online_total_total`, ...).
- scrape target health: <http://localhost:9090/targets>, the `quicmetrics` job
  should be **UP**

Follow the server logs and stop the suite:

    docker compose logs -f quicmetrics
    make compose-down

The server listens on `localhost:4242` by default. Use the `-addr` flag to bind
another address, as the compose suite does with `-addr 0.0.0.0:4242`.

# quicmetrics

Receive and aggregate fleeting metrics (from FaaS).

This proof of concept uses:
- [QUIC](https://peering.google.com/#/learn-more/quic) without http3 as a transport protocol
- statsd string format for metrics

The `quicmetrics-client` example sends several counters, so you can check that they are
summed in the Prometheus endpoint output.

Libraries used:
- [quic-go](https://github.com/quic-go/quic-go), a QUIC implementation in pure go
- [go-dogstatsd-parser](https://github.com/narqo/go-dogstatsd-parser), a standalone parser for DogStatsD metrics protocol
- [prometheus/client_golang](https://github.com/prometheus/client_golang), the Go client library for prometheus
- [opentelemetry-go](https://github.com/open-telemetry/opentelemetry-go), the Go implementation of OpenTelemetry

## Layout

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, graceful shutdown |
| `cmd/quicmetrics-client` | Example client that sends sample counters |
| `internal/server` | QUIC sessions, DogStatsD parsing, OpenTelemetry aggregation, TLS |
| `internal/protocol` | ALPN and default address shared by server and client |

## Getting started

Launch the server

    go run ./cmd/quicmetrics

Send sample metrics

    go run ./cmd/quicmetrics-client

View prometheus metrics

    curl localhost:2223/metrics

Server flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `localhost:4242` | UDP address of the QUIC listener |
| `-metrics-addr` | `:2223` | TCP address of the Prometheus `/metrics` endpoint |
| `-log-level` | `info` | `debug` also logs every received metric |

The client accepts `-addr` (default `localhost:4242`). The server stops gracefully on
`SIGINT` or `SIGTERM`.

## Development

    make check   # fmt, vet, golangci-lint, tests with -race
    make vuln    # govulncheck
    make build   # binaries in build/

`make lint` needs [golangci-lint](https://golangci-lint.run/welcome/install/) v2
(CI pins v2.15.0).

## Run with docker compose

The [compose.yaml](compose.yaml) suite runs the server together with a
[Prometheus](https://prometheus.io) instance that scrapes it every 5 seconds
(see [prometheus/prometheus.yml](prometheus/prometheus.yml)).

| Service | Port on the host | Purpose |
| --- | --- | --- |
| `quicmetrics` | `4242/udp` | QUIC listener receiving metrics |
| `quicmetrics` | `2223/tcp` | Prometheus `/metrics` endpoint |
| `prometheus` | `9090/tcp` | Prometheus UI |

Start the suite. The image is built from source in a multi-stage Docker build and runs as a
non-root distroless container:

    make compose-up

Send sample metrics from your machine to the server running in docker compose:

    go run ./cmd/quicmetrics-client

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

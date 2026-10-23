# quicmetrics

Receive and aggregate fleeting metrics (from FaaS).

This proof of concept uses:
- [QUIC](https://peering.google.com/#/learn-more/quic) without http3 as a transport protocol
- statsd string format for metrics

The `quicmetrics-client` generates realistic FaaS traffic (invocations, HTTP requests,
billed time, errors), so you can watch the counters add up in the Prometheus endpoint output.

Libraries used:
- [quic-go](https://github.com/quic-go/quic-go), a QUIC implementation in pure go
- [go-dogstatsd-parser](https://github.com/narqo/go-dogstatsd-parser), a standalone parser for DogStatsD metrics protocol
- [prometheus/client_golang](https://github.com/prometheus/client_golang), the Go client library for prometheus
- [opentelemetry-go](https://github.com/open-telemetry/opentelemetry-go), the Go implementation of OpenTelemetry

## Layout

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, graceful shutdown |
| `cmd/quicmetrics-client` | Client that generates realistic counters |
| `internal/server` | QUIC sessions, DogStatsD parsing, OpenTelemetry aggregation, TLS |
| `internal/protocol` | ALPN and default address shared by server and client |

## Getting started

Launch the server

    go run ./cmd/quicmetrics

Generate metrics for 10 seconds at 10 metrics per second

    go run ./cmd/quicmetrics-client

View prometheus metrics

    curl localhost:2223/metrics

Server flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `localhost:4242` | UDP address of the QUIC listener |
| `-metrics-addr` | `:2223` | TCP address of the Prometheus `/metrics` endpoint |
| `-log-level` | `info` | `debug` also logs every received metric |

The server stops gracefully on `SIGINT` or `SIGTERM`.

Client flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `localhost:4242` | UDP address of the quicmetrics server |
| `-rate` | `10` | Metrics per second, from 0.001 to 1e9 (the first is sent right away). `0` sends as fast as acks come back |
| `-duration` | `10s` | Stop this long after connecting, `0` runs until `SIGINT` or `SIGTERM` |
| `-count` | `0` | Stop after this many metrics, `0` means no limit |
| `-functions` | `5` | Number of simulated functions (1-10), drives tag cardinality |
| `-seed` | `0` | Random seed to replay a run, `0` picks one (logged at start) |
| `-log-level` | `info` | `debug` also logs every acknowledged metric |

The client sends four counters, tagged by `function` and `region`: `faas.invocations`
(with `cold_start`), `http.requests` (with `method` and `status`), `faas.billed.ms` and
`faas.errors` (with `error`). A few functions get most of the traffic, most requests succeed,
and errors stay rare. With all 10 functions, `http.requests` has at most 600 tag sets, well
under the server's limit of 2000 per counter.

### Client examples

Send 400 metrics at 200 per second across 4 functions. With a fixed seed, a fresh server
always ends up with the same 82 series:

    $ go run ./cmd/quicmetrics-client -rate 200 -count 400 -functions 4 -seed 7
    level=INFO msg="generating metrics" addr=localhost:4242 seed=7 rate=200 count=400 duration=10s functions=4
    level=INFO msg="metrics acknowledged" count=400 elapsed=1.996s

    $ curl -s localhost:2223/metrics | grep -E '^(faas|http)'
    ...
    faas_billed_ms_total{function="checkout",region="eu-west-1"} 1434
    faas_errors_total{error="timeout",function="checkout",region="us-east-1"} 4
    faas_invocations_total{cold_start="false",function="checkout",region="us-east-1"} 123
    http_requests_total{function="checkout",method="GET",region="us-east-1",status="200"} 159
    ...

Other runs:

    # Defaults: 10 metrics/s for 10s, 5 functions, random seed (logged at start)
    go run ./cmd/quicmetrics-client

    # Keep sending until Ctrl+C, with all 10 functions (most tag sets)
    go run ./cmd/quicmetrics-client -rate 200 -duration 0 -functions 10

    # Exactly 1000 metrics, as fast as the acks come back
    go run ./cmd/quicmetrics-client -rate 0 -count 1000

    # Replay a run, and log every metric as it is acknowledged
    go run ./cmd/quicmetrics-client -seed 7 -count 5 -log-level debug

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

Generate metrics from your machine to the server running in docker compose:

    go run ./cmd/quicmetrics-client

Check the metrics:

- raw endpoint: `curl localhost:2223/metrics`
- Prometheus UI: open <http://localhost:9090/query>, query
  `sum by (status) (rate(http_requests_total[1m]))` and click **Execute**. Switch to the
  **Graph** tab and run the client with `-duration 0` to watch the request rate by status.
- scrape target health: <http://localhost:9090/targets>, the `quicmetrics` job
  should be **UP**

Follow the server logs and stop the suite:

    docker compose logs -f quicmetrics
    make compose-down

The server listens on `localhost:4242` by default. Use the `-addr` flag to bind
another address, as the compose suite does with `-addr 0.0.0.0:4242`.

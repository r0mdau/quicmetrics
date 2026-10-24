# quicmetrics

Receive and aggregate fleeting metrics (from FaaS).

This proof of concept uses:
- [QUIC](https://peering.google.com/#/learn-more/quic) without http3 as a transport protocol
- statsd string format for metrics

The `quicmetrics-client` generates realistic FaaS traffic (invocations, HTTP requests,
billed time, errors, in-flight executions, memory use, execution time, request latency), so you
can watch the counters add up, the gauges move and the histograms fill their buckets in the
Prometheus endpoint output.

Libraries used:
- [quic-go](https://github.com/quic-go/quic-go), a QUIC implementation in pure go
- [go-dogstatsd-parser](https://github.com/narqo/go-dogstatsd-parser), a standalone parser for DogStatsD metrics protocol
- [prometheus/client_golang](https://github.com/prometheus/client_golang), the Go client library for prometheus
- [opentelemetry-go](https://github.com/open-telemetry/opentelemetry-go), the Go implementation of OpenTelemetry

## Layout

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, graceful shutdown |
| `cmd/quicmetrics-client` | Client that generates realistic counters, gauges and histograms |
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
and errors stay rare. It also sends two gauges on about 13% of the lines, tagged by `function`
and `region`: `faas.concurrency`, the in-flight executions (a whole number that drifts by at
most one between samples of a series) and `faas.memory.utilization`, a ratio from 0 to 1.
Two histograms take another 15% of the lines, with long-tailed values in milliseconds:
`faas.duration` (`|h`, tagged by `function` and `region`), the execution time, mostly tens to
hundreds of ms with a tail into seconds and 5% of slow executions (4 times longer), and
`http.latency` (`|ms`, also tagged by `method`), the request latency. With all 10 functions,
`http.requests` has at most 600 tag sets, well under the server's limit of 2000 per counter,
gauge or histogram.

A counter adds up, so Prometheus exposes it with a `_total` suffix. A gauge (`|g`) keeps only
the last value received for each series and is exposed as is, without `_total`. A gauge value
can be any finite number, and a leading sign is an absolute value, not a delta. A sample rate
(`|@0.5`) is validated like for counters but never scales a gauge. A gauge keeps its last value on `/metrics` after the client stops
sending it.

A histogram (`|h`, or `|ms` for a timer) records each value in a distribution. Both types
with the same name share one series, and Prometheus exposes them without a unit suffix as
`foo_bucket{le="..."}`, `foo_sum` and `foo_count`. The buckets are the OpenTelemetry defaults
(0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500 and 10000), suited to
milliseconds, and cannot be configured. A value can be any finite number up to 2^63 in
magnitude (a timer cannot be negative). A sample rate (`|@0.5`) is validated like for counters
but never scales a histogram: `_count` and `_sum` only cover the values actually received.

### Client examples

Send 400 metrics at 200 per second across 4 functions. With a fixed seed, a fresh server
always ends up with the same 81 counter series (plus 20 gauge series and 24 histogram series):

    $ go run ./cmd/quicmetrics-client -rate 200 -count 400 -functions 4 -seed 7
    level=INFO msg="generating metrics" addr=localhost:4242 seed=7 rate=200 count=400 duration=10s functions=4
    level=INFO msg="metrics acknowledged" count=400 elapsed=1.996s

    $ curl -s localhost:2223/metrics | grep -E '^(faas|http)'
    ...
    faas_billed_ms_total{function="checkout",region="eu-west-1"} 1648
    faas_errors_total{error="timeout",function="checkout",region="us-east-1"} 3
    faas_invocations_total{cold_start="false",function="checkout",region="us-east-1"} 84
    http_requests_total{function="checkout",method="GET",region="us-east-1",status="200"} 125
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
    make bench   # benchmarks with allocation stats

`make bench` runs every benchmark in `internal/server` (parsing, recording, cardinality,
Prometheus scrapes, QUIC sessions). Run a subset with
`go test -run '^$' -bench Scrape -benchmem ./internal/server`. To compare two versions, save
each run with `go test -run '^$' -bench . -benchmem -count 10 ./... > old.txt` (then `new.txt`)
and run [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) `old.txt new.txt`.
CI runs each benchmark once to keep them compiling and passing, not to time them.

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
  For the 95th percentile of the function execution time, query
  `histogram_quantile(0.95, sum by (le) (rate(faas_duration_bucket[1m])))`.
- scrape target health: <http://localhost:9090/targets>, the `quicmetrics` job
  should be **UP**

Follow the server logs and stop the suite:

    docker compose logs -f quicmetrics
    make compose-down

The server listens on `localhost:4242` by default. Use the `-addr` flag to bind
another address, as the compose suite does with `-addr 0.0.0.0:4242`.

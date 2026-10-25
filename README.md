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
| `internal/server` | QUIC listener and sessions, DogStatsD parsing, OpenTelemetry aggregation, TLS certificates |
| `internal/tlstest` | Throwaway certificates for tests |
| `internal/protocol` | ALPN and default address shared by server and client |

## Wire protocol

A client opens a QUIC connection with the ALPN from `internal/protocol` and one
bidirectional stream (the server allows no second stream and no unidirectional stream). A
stream is a byte stream, so metrics are newline-delimited
[DogStatsD](https://docs.datadoghq.com/developers/dogstatsd/datagram_shapes/) lines:

- a line ends with `\n`. A trailing `\r` is dropped, empty lines are skipped and a last line
  without `\n` is processed when the client closes its side of the stream. Several metrics can
  share one write and one metric can span several writes.
- a line is at most 8192 bytes (`server.MaxLineLength`) without its terminator. A longer line
  closes the session with application error code 1 (see below).
- the server never echoes metrics. It acknowledges them in bulk: after it has processed every
  complete line it has read, and before it waits for more bytes, it writes one line holding the
  number of non-empty lines it has processed on the stream so far, in decimal (`1000\n`). A
  line counts whether it was recorded or logged and dropped (invalid, unsupported type, over a
  limit), but an empty line does not, and neither does an incomplete line until the client closes its
  side of the stream. Acks are strictly increasing, at
  most one per chunk the server reads, and none is sent when nothing new was processed.
- a client can therefore pipeline: write as many lines as it wants without waiting, and read
  acks as they come. Ack `N` means the first `N` non-empty lines it wrote are processed, so
  `sent - N` are still in flight. `quicmetrics-client` keeps at most 4 times `-batch` metrics in
  flight so a slow server holds the client back.
- a client must read its last ack before it closes the stream or connection: an ack written
  just before the session closes may be lost, and so may the acks for lines processed just
  before a `line too long` close. The metrics are recorded either way.

The server closes a connection with one of these QUIC application error codes (defined in
`internal/protocol/errors.go`), and the close reason says why:

| Code | Name | Meaning |
| --- | --- | --- |
| 0 | `CodeNoError` | Normal close: the client is done, or the server is shutting down |
| 1 | `CodeLineTooLong` | A line exceeds 8192 bytes (reason `line too long`) |
| 2 | `CodeServerBusy` | The server already serves `-max-sessions` sessions (reason `too many sessions`) |

## Getting started

Launch the server. Without `-tls-cert` and `-tls-key` it generates a self-signed certificate
and warns about it, which is for development only (see [TLS](#tls))

    go run ./cmd/quicmetrics

Generate metrics for 10 seconds at 10 metrics per second. The client verifies the server
certificate, so it needs `-insecure` to accept the self-signed one of a development server

    go run ./cmd/quicmetrics-client -insecure

View prometheus metrics

    curl localhost:2223/metrics

Server flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `localhost:4242` | UDP address of the QUIC listener |
| `-metrics-addr` | `:2223` | TCP address of the Prometheus `/metrics` endpoint |
| `-idle-timeout` | `30s` | A session without traffic for this long is closed |
| `-max-sessions` | `1000` | Concurrent QUIC sessions, further connections are closed with application error code 2 (`too many sessions`) |
| `-tls-cert` | | PEM certificate chain of the QUIC listener, reloaded when the file changes. Needs `-tls-key` |
| `-tls-key` | | PEM private key of the QUIC listener. Needs `-tls-cert` |
| `-tls-reload-interval` | `30s` | How often `-tls-cert` and `-tls-key` are checked for changes |
| `-log-level` | `info` | `debug` also logs every received metric |

The server stops gracefully on `SIGINT` or `SIGTERM`.

The listener is hardened against misbehaving peers: a connection may open one bidirectional
stream and no unidirectional stream, per-stream and per-connection receive windows are small
(256 KiB and 512 KiB at most), and every client proves it owns its source address with a QUIC
Retry round trip before the server keeps any handshake state for it, so spoofed addresses cost
the server nothing. A client that stays silent for `-idle-timeout` is dropped, so a client sending
less often than that needs a keep-alive.

Client flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `localhost:4242` | UDP address of the quicmetrics server |
| `-rate` | `10` | Metrics per second, from 0.001 to 1e9 (the first is sent right away), a target the client cannot exceed but may miss when acks hold it back. `0` sends as fast as acks come back, 4 batches ahead |
| `-batch` | `100` | Maximum metrics per write, from 1 to 10000. Metrics that come due together (a high `-rate`) share a write, so a low rate sends one metric per write |
| `-duration` | `10s` | Stop sending this long after connecting (then wait up to 5s for the last acks), `0` runs until `SIGINT` or `SIGTERM` |
| `-count` | `0` | Stop after this many metrics, `0` means no limit |
| `-functions` | `5` | Number of simulated functions (1-10), drives tag cardinality |
| `-seed` | `0` | Random seed to replay a run, `0` picks one (logged at start) |
| `-tls-ca` | system roots | PEM bundle of the CAs that sign the server certificate |
| `-tls-server-name` | host of `-addr` | Name the server certificate must be valid for |
| `-insecure` | `false` | Do not verify the server certificate, with a warning. Development only. Excludes `-tls-ca` |
| `-log-level` | `info` | `debug` also logs every metric sent |

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

### TLS

QUIC always runs over TLS 1.3. The server presents the certificate of `-tls-cert` and
`-tls-key` (PEM files, for example a Kubernetes Secret managed by cert-manager and mounted
as files). Every `-tls-reload-interval` it re-reads both files and, if their content changed,
serves the new certificate to new connections without a restart. Open sessions keep the one
they were set up with. This works with the atomic symlink swap the kubelet does when a Secret
is updated. If the new pair can't be loaded (unreadable, invalid, or a certificate that doesn't
match its key, as can happen for one interval in the middle of a swap), the server logs a
warning, keeps the previous certificate and tries again when the content changes. At start
the files must be valid, otherwise the server exits.

Without `-tls-cert` and `-tls-key` the server generates a self-signed certificate at every
start and logs a warning: clients can't verify it, so this is for development only. Setting
only one of the two flags is an error.

The client verifies the server certificate against the CAs of `-tls-ca`, or the system roots
when the flag is not set, and against the host of `-addr`, or `-tls-server-name` when the
address is an IP or a name the certificate doesn't cover. **Verification is on by default**: a
client without `-tls-ca` fails to connect to a server using a private CA or the self-signed
fallback with `certificate signed by unknown authority`. Only `-insecure` turns it off, with a
warning, and it can't be combined with `-tls-ca`.

A private CA and server certificate for local tests, with openssl:

    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 \
      -subj /CN=dev-ca -addext basicConstraints=critical,CA:TRUE -keyout ca.key -out ca.pem
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
      -subj /CN=localhost -keyout tls.key -out tls.csr
    openssl x509 -req -in tls.csr -CA ca.pem -CAkey ca.key -days 30 -out tls.crt \
      -extfile <(printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth')
    go run ./cmd/quicmetrics -tls-cert tls.crt -tls-key tls.key
    go run ./cmd/quicmetrics-client -tls-ca ca.pem

### Client examples

The examples run against a development server (self-signed certificate), hence `-insecure`;
with a real certificate use `-tls-ca` or the system roots instead.

Send 400 metrics at 200 per second across 4 functions. With a fixed seed, a fresh server
always ends up with the same 81 counter series (plus 20 gauge series and 24 histogram series):

    $ go run ./cmd/quicmetrics-client -insecure -rate 200 -count 400 -functions 4 -seed 7
    level=WARN msg="-insecure: the server certificate is not verified, for development only"
    level=INFO msg="generating metrics" addr=localhost:4242 seed=7 rate=200 count=400 duration=10s batch=100 functions=4
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
    go run ./cmd/quicmetrics-client -insecure

    # Keep sending until Ctrl+C, with all 10 functions (most tag sets)
    go run ./cmd/quicmetrics-client -insecure -rate 200 -duration 0 -functions 10

    # Exactly 1000 metrics, as fast as the acks come back
    go run ./cmd/quicmetrics-client -insecure -rate 0 -count 1000

    # Replay a run, and log every metric sent
    go run ./cmd/quicmetrics-client -insecure -seed 7 -count 5 -log-level debug

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

Generate metrics from your machine to the server running in docker compose. The suite doesn't
set `-tls-cert` and `-tls-key`, so the server uses its self-signed development certificate:

    go run ./cmd/quicmetrics-client -insecure

To use your own certificate instead, mount the files and add the flags to `command` in
[compose.yaml](compose.yaml) (the files are read by the non-root container user):

    volumes:
      - ./certs:/etc/quicmetrics:ro
    command: ["-addr", "0.0.0.0:4242", "-tls-cert", "/etc/quicmetrics/tls.crt", "-tls-key", "/etc/quicmetrics/tls.key"]

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

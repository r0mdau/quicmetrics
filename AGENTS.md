# AGENTS.md

Guidance for AI coding agents working in this repository.

## Project overview

`quicmetrics` is a small Go **proof of concept** that receives short-lived metrics
(e.g. from FaaS workloads) over **raw QUIC** (no HTTP/3), parses them as
**DogStatsD** strings, aggregates them with the **OpenTelemetry** metrics SDK, and
exposes them through a **Prometheus** `/metrics` endpoint.

| Path | Role |
| --- | --- |
| `main.go` | Server: QUIC listener + DogStatsD parsing + OTel aggregation + Prometheus HTTP endpoint |
| `client/main.go` | Example client that sends a few sample counters over one QUIC stream |
| `Makefile` | fmt / vet / lint / build / test / docker-build / compose-up / compose-down targets |
| `Dockerfile` | Packages the prebuilt `build/quicmetrics` binary |
| `compose.yaml` | Docker compose suite: server + Prometheus scraping it |
| `prometheus/prometheus.yml` | Prometheus scrape config used by the compose suite |
| `.github/workflows/ci.yml` | On push to `main`: gofmt check, vet, `go test -race`, build |

There is a single Go module (`github.com/r0mdau/quicmetrics`), two `main` packages, and no
internal libraries.

## Toolchain: read this first

- `go.mod` targets **Go 1.26** and uses `github.com/quic-go/quic-go v0.64.1`, which itself
  requires Go ≥ 1.26. The old `github.com/lucas-clemente/quic-go` path must not come back.
- In this quic-go version, `quic.Stream` is a **struct** (use `*quic.Stream`), and
  `quic.DialAddr` takes a `context.Context` as first argument.
- The Docker image is based on `golang:1.26-trixie`.
- The OTel metric packages are **pre-1.0 (`v0.33.0`)**. The code uses APIs that were removed
  later (`meter.SyncFloat64()`, the `go.opentelemetry.io/otel/metric/instrument` package). Don't
  bump OTel versions as a side effect of other work.

## Common commands

```sh
make            # default = build (runs fmt -> vet -> build) into ./build/
make fmt        # go fmt ./...
make vet        # go vet ./...
make lint       # golint ./...  (needs golint on PATH)
make test       # go test -cover ./...
make verify     # fmt + test
make docker-build   # build, then docker build -t r0mdau/quicmetrics .
make compose-up     # build, then docker compose up --build -d (server + Prometheus on :9090)
make compose-down   # docker compose down
```

Run locally (3 terminals):

```sh
go run main.go              # server: QUIC on localhost:4242 (UDP), metrics on :2223
go run client/main.go       # send sample counters
curl localhost:2223/metrics # check aggregated output, e.g. users_online_total{...}
```

## Architecture and protocol details

- **Ports**: the server's QUIC listener defaults to `localhost:4242` (UDP) and can be changed
  with the `-addr` flag. The client dials the hardcoded `addr` const `localhost:4242` in
  `client/main.go`, so keep the default and the client in sync. Prometheus HTTP listens on
  `:2223`. The compose suite runs the server with `-addr 0.0.0.0:4242` so the host port
  mapping works, and exposes Prometheus on `:9090`.
- **TLS**: the server makes a throwaway self-signed RSA cert at every startup
  (`generateTLSConfig`). The client uses `InsecureSkipVerify: true`.
- **ALPN**: both sides must use the same `NextProtos` value, currently `"quic-echo-example"`.
  If you change one, change the other.
- **Streams**: the server accepts **one stream per connection** and runs `io.Copy` through
  `loggingWriter`. Each `Write` is parsed and then **echoed back** to the client, which acts as
  an ack.
- **No message framing**: each `Write` call is treated as exactly one metric. This only works
  because the client waits for the echo (`io.ReadFull`) before sending the next metric. If
  several metrics end up in one read buffer, parsing fails. Keep this in mind before changing
  how the client sends metrics.
- **Metric types**: only DogStatsD **counters** (`|c`) are aggregated. They become OTel
  `Float64Counter`s, with DogStatsD tags as attributes. Other types are parsed and echoed, but
  not recorded. Invalid lines are logged, skipped for aggregation, and still echoed.

## Known pitfalls

- The DogStatsD parser truncates counter values to `int64`, so `foo:1.5|c` is silently
  recorded as `1`. Sample rates (`|@0.5`) are ignored.
- `dogstatsd.Parse` panics on an empty trailing segment (e.g. `foo:1|c|`). Nothing recovers
  it, so one malformed metric crashes the server.
- The OTel Prometheus exporter `v0.33.0` appends `_total` once per data point, so a counter
  with several attribute sets is exported as `x_total`, `x_total_total`, and so on. Fixing it
  requires an OTel upgrade.
- The global `meter` is set inside the `serveMetrics` goroutine, so it is nil if a metric
  arrives before that goroutine has run.
- `log.Fatal` on instrument creation errors kills the whole server.
- If `AcceptStream` fails, the code prints the error and continues with a nil `*quic.Stream`,
  so `io.Copy` panics and crashes the server.
- `main_test.go` holds **contract tests** (echo, counter aggregation, ignored input, Prometheus
  exposition). They deliberately don't pin the quirks above, so they should keep passing when
  those are fixed. Add new `_test.go` files next to the code they cover.

## Conventions

- Format with `gofmt` (`make fmt`) and keep `go vet` clean before you finish.
- **Inline comments**: when you change code, don't add inline comments. Only add one when the
  logic is genuinely complicated and can't be made clear through naming or structure. Godoc
  comments on exported identifiers are fine. Leave existing comments in place.
- Use the standard library `log` package for logging. Don't add a logging framework.
- Keep the PoC small: avoid new dependencies unless the task needs them.
- `build/` and `vendor/` are gitignored. Never commit binaries.
- The repo is Go-only. The root `package.json` and `package-lock.json` are stray untracked
  files, so don't add JS tooling around them.
- License: Apache-2.0 (`LICENSE`).

## Second-pass review for large changes

When a change edits a large portion of the codebase, have a **new coding agent with fresh
context** (one that didn't write the code) review the result before you call the task done.
Examples of a large change: rewriting most of `main.go`, changing both `main` packages, the
protocol, or dependencies, or a migration such as moving to `quic-go/quic-go`.

- Give the reviewer the goal, the diff (`git diff`), and this file. Don't give it your reasoning,
  so its judgment stays independent.
- Ask it to check correctness, regressions against the protocol details above, the known
  pitfalls, and these conventions (including the inline comment rule).
- Fix what it finds, re-run `make fmt`, `make vet`, and `make test`, and summarize the review
  outcome to the user.

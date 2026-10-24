# AGENTS.md

`quicmetrics` is a small Go proof of concept: it receives DogStatsD metrics over raw QUIC
(no HTTP/3), aggregates them with the OpenTelemetry SDK, and exposes them on a Prometheus
`/metrics` endpoint. Compose (`compose.yaml`) adds Prometheus on `:9090`.

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, `run(ctx, args, stderr) error` |
| `cmd/quicmetrics-client` | Client that generates realistic counters (`generator.go`) |
| `internal/server` | `Server` (QUIC sessions), `Aggregator` + `Parse`, `SelfSignedTLSConfig` |
| `internal/protocol` | `ALPN` and `DefaultAddr`, shared by both commands |

## Commands

```sh
make check                          # fmt, vet, golangci-lint v2, test -race: run before you finish
make bench                          # benchmarks, not part of check; compare runs with benchstat
make vuln                           # govulncheck
make compose-up / compose-down      # server + Prometheus in Docker
go run ./cmd/quicmetrics            # QUIC on localhost:4242/udp, metrics on :2223
go run ./cmd/quicmetrics-client     # generate counters for 10s (see -h for flags)
```

## Dependencies

- Go **1.26** with `github.com/quic-go/quic-go v0.64.1`: `*quic.Stream` and `*quic.Conn`
  are structs, and `quic.DialAddr` takes a `context.Context`. Never bring back
  `lucas-clemente/quic-go`.
- OTel uses the stable metric API (`go.opentelemetry.io/otel` v1.47, Prometheus exporter
  v0.69). All exporter options live in `server.NewPrometheusMeterProvider`: Prometheus-style
  names (`users.online` becomes `users_online_total`) and no `otel_scope_*` labels. Keep them
  there so tests and production expose the same series.

## Protocol rules

- Client and server take the ALPN and default address from `internal/protocol`. Never
  hardcode them. The container defaults to `-addr 0.0.0.0:4242`.
- TLS: the server generates a self-signed P-256 certificate at every start, and the client
  uses `InsecureSkipVerify`.
- The server accepts **one stream per connection** and echoes every `Write` back as an ack.
- **No framing**: one `Write` is one metric. This only works because the client waits for
  each echo before sending the next metric. Several metrics in one read fail to parse.
- Only counters (`|c`) are recorded, with tags as attributes. Invalid lines are logged as
  warnings, and other metric types at debug level. Both are echoed, but not recorded.
- Counter values are recorded as float64 (`1.5` stays `1.5`). `NaN` and values above 2^63
  in magnitude (including `±Inf`) are logged and dropped: the SDK sums them unchecked, and a
  `NaN` or `Inf` sum would stick to the series forever.
- Sample rates: `foo:1|c|@0.1` records `1/0.1 = 10`. `server.Parse` reads the rate from the raw
  line as float64 (the parser's float32 would give 9.99999985), divides the value by it, and
  returns the scaled value. `Metric.Rate` is left as parsed and must not be applied again. A
  rate outside (0, 1] is logged and the metric dropped: `dogstatsd.Parse` lets `@0` and `@NaN`
  through, and dividing by them gives `Inf` or `NaN`. The 2^63 bound applies to the scaled value
  too (`@1e-50` and `@5e-324` overflow it), and a rate that underflows float32 is judged by its
  float64 value.
- Metric names must be valid OTel instrument names (start with a letter, then
  `[A-Za-z0-9_.-/]`, up to 255 characters). Other names are logged and dropped.
- Tag keys need an ASCII letter or digit and must not start with `__`, otherwise the whole
  metric is logged and dropped: the exporter can't turn such keys into label names.

## Crash safety

One client must never take the server down.

- `panic` and fatal exits are only allowed at startup, in `cmd/`. Per-metric errors are
  logged and the metric is dropped. `handleConn` recovers panics within its own session.
- Always parse with `server.Parse`, never `dogstatsd.Parse`. It recovers the parser's panic on
  empty segments (`foo:1|c|`) and rejects invalid UTF-8, which would otherwise make every
  later scrape log an error and drop the series (HTTP 500 without `ContinueOnError`). It also
  re-parses counter values, which `dogstatsd.Parse` truncates to `int64`, and sample rates.
- client_golang recovers panics in collectors, but a panic still drops every OTel series on
  every scrape. Validate any new client-controlled data before it reaches OTel.
- `/metrics` uses `promhttp.ContinueOnError`: colliding series (e.g. `a.b` and `a_b`, or `foo`
  and `foo.total`) are logged and skipped instead of turning every scrape into an HTTP 500.

## Known quirks (not yet fixed)

- Negative counter values are recorded, so a counter can go down (Prometheus reads that as
  a reset).
- Each counter keeps at most 2000 tag sets (`NewPrometheusMeterProvider`). Beyond that,
  values go to a single `otel_metric_overflow="true"` series.

## Conventions

- **No package-level mutable state**: inject dependencies (meter, `*slog.Logger`) through
  constructors so tests can run with `t.Parallel()`.
- Log with `log/slog` and an injected logger, using key/value attributes. Don't add a logging
  library.
- Return errors wrapped with context (`fmt.Errorf("...: %w", err)`) instead of logging and
  continuing, except at the per-metric and per-session boundaries above.
- One `_test.go` per source file, in the same package. Contract tests in
  `aggregator_test.go` deliberately don't pin the quirks above. Context goes first in helper
  parameters.
- Don't add inline comments unless the logic is genuinely hard to follow. Keep existing
  comments. Exported identifiers and packages need godoc.
- Benchmarks sit beside the code they measure, use `b.Loop()` and the `testing.TB` helpers,
  and assert their result once after the loop so they never time an error path.
  `TestRecordAllocations` pins the allocations of `Aggregator.Record`: lower the budget when
  you remove allocations, and justify raising it.
- Avoid new dependencies. Never commit `build/` or binaries.

## Review large changes

For large changes (a whole package, both commands, the protocol, or dependencies), have a
fresh agent review before you finish. Give it the goal, `git diff`, and this file, but not
your reasoning. Fix what it finds, re-run `make check`, and report the outcome to the user.

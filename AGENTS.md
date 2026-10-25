# AGENTS.md

`quicmetrics` is a small Go proof of concept: it receives DogStatsD metrics over raw QUIC
(no HTTP/3), aggregates them with the OpenTelemetry SDK, and exposes them on a Prometheus
`/metrics` endpoint. Compose (`compose.yaml`) adds Prometheus on `:9090`.

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, `run(ctx, args, stderr) error` |
| `cmd/quicmetrics-client` | Client that generates realistic counters, gauges and histograms (`generator.go`) and pipelines them in batches |
| `internal/server` | `Server` (QUIC sessions), `Aggregator` (counters, gauges, histograms) + `Parse`, `SelfSignedTLSConfig` |
| `internal/protocol` | `ALPN` and `DefaultAddr`, shared by both commands |

## Commands

```sh
make check                          # fmt, vet, golangci-lint v2, test -race: run before you finish
make bench                          # benchmarks, not part of check; compare runs with benchstat
make vuln                           # govulncheck
make compose-up / compose-down      # server + Prometheus in Docker
go run ./cmd/quicmetrics            # QUIC on localhost:4242/udp, metrics on :2223
go run ./cmd/quicmetrics-client     # generate all metric types for 10s (see -h for flags)
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
- The server accepts **one stream per connection**. The stream is a byte stream, so metrics
  are **newline-delimited**: a line ends with `\n`, a trailing `\r` is dropped, empty lines are
  skipped (and not acknowledged), and a last line without `\n` is processed when the client
  closes its side of the stream. Reads may split a line or carry several, the server never
  depends on how the client chunks its writes.
- A line is at most `server.MaxLineLength` (8 KiB) without its terminator. A longer line
  closes the session with QUIC application error code 1 (`line too long`) and a warning,
  rather than dropping it, because the client is broken and the server would have to discard
  an unbounded stream to resynchronise. Lines acknowledged before that stay recorded.
- **Acks are cumulative counts, the server never echoes metrics.** After it has processed every
  complete line it has read, and before it blocks for more bytes, the server writes the number
  of non-empty lines processed on the stream so far, in ASCII decimal, terminated by `\n`
  (`1000\n`). The count includes lines that were invalid, ignored or dropped, never empty
  lines, nor an incomplete line until the client closes its side of the stream. Acks are
  strictly increasing, at most one per read chunk, and none is written when nothing new was
  processed. A client can write any number of lines without waiting: ack `N` means the first
  `N` non-empty lines it wrote are processed, so `sent - N` are in flight. The client keeps at
  most `windowBatches` (4) times `-batch` metrics in flight.
- An ack written just before the session closes (client FIN, then connection close) may be
  lost, and so may the ack for lines processed just before a `line too long` close. A client
  must read its last ack before it closes. The client does: once it stops sending, it waits up
  to 5 seconds for the missing acks and then fails.
- Only counters (`|c`), gauges (`|g`), histograms (`|h`) and timers (`|ms`) are recorded, with
  tags as attributes. Invalid lines are logged as warnings, and other metric types (sets
  `|s`, ...) at debug level. Both are acked, but not recorded.
- Gauges: `temperature:21.5|g|#room:a` records the last value per series (name and tag set) in
  an OTel `Float64Gauge` from the same meter, exported without a `_total` suffix
  (`temperature{room="a"} 21.5`). Any finite float64 is valid, with no 2^63 bound since nothing
  is summed. `NaN` and `±Inf` are logged and dropped. A sign (`+3`, `-3`) is an absolute value,
  not a delta: DogStatsD has no relative gauges. The sample rate is validated like for counters
  (outside (0, 1] is dropped) but not applied to the value. Names, tag keys, UTF-8 and the
  cardinality limit follow the same rules and code paths as counters.
- Histograms: `latency:12|h|#route:a` and `latency:12|ms|#route:a` (a timer, which DogStatsD
  treats as a histogram) are recorded in an OTel `Float64Histogram` from the same meter. Both
  types with the same name share one instrument and one series per tag set. Prometheus gets a
  classic histogram without unit suffix (`latency_bucket{le="..."}`, `latency_sum`,
  `latency_count`) with the OTel SDK default buckets `0, 5, 10, 25, 50, 75, 100, 250, 500, 750,
  1000, 2500, 5000, 7500, 10000` (milliseconds), and a value equal to a bound goes in that
  bound's bucket (`le="5"` holds 5). There are no per-metric buckets. The value must be finite
  with a magnitude of at most 2^63 like for counters, which keeps `NaN`, `±Inf` and absurd
  single values out of the accumulating `_sum` (it does not bound the running total, see the
  known quirks): `NaN`, `±Inf` and larger values are logged and dropped. A negative `|h` is
  accepted (it lands in `le="0"` and lowers `_sum`); the parser already rejects a negative
  `|ms`. The sample rate is validated like for counters (outside (0, 1] is dropped) but not
  applied. Names, tag keys, UTF-8 and the cardinality limit follow the same rules and code
  paths as counters and gauges.
- Counter values are recorded as float64 (`1.5` stays `1.5`). `NaN` and values above 2^63
  in magnitude (including `±Inf`) are logged and dropped: the SDK sums them unchecked, and a
  `NaN` or `Inf` sum would stick to the series forever. The bound applies to each value, not to
  the running total (see the known quirks).
- Sample rates: `foo:1|c|@0.1` records `1/0.1 = 10`. `server.Parse` reads the rate from the raw
  line as float64 (the parser's float32 would give 9.99999985), divides the value by it, and
  returns the scaled value. `Metric.Rate` is left as parsed and must not be applied again. A
  rate outside (0, 1] is logged and the metric dropped: `dogstatsd.Parse` lets `@0` and `@NaN`
  through, and dividing by them gives `Inf` or `NaN`. The 2^63 bound applies to the scaled value
  too (`@1e-50` and `@5e-324` overflow it), and a rate that underflows float32 is judged by its
  float64 value.
- A counter and a gauge with the same name coexist when their Prometheus names differ (`foo|c`
  is `foo_total`, `foo|g` is `foo`). `foo.total|g` collides with `foo|c`: the exporter logs
  `instrument type conflict` and only the series of the first instrument created is exposed
  (which one that is depends on the order sessions send them); a plain `promhttp` handler
  would answer HTTP 500, so keep `ContinueOnError`.
  `TestCounterAndGaugeWithTheSameNameKeepScrapesWorking` pins this.
  A histogram `foo` exposes the family `foo` with the series `foo_bucket`, `foo_sum` and
  `foo_count`, so it coexists with a counter `foo` (`foo_total`) but collides with a gauge
  `foo` (same family) and with a gauge `foo.count` or `foo.sum` (`foo_count`, `foo_sum`). Same
  outcome as above: one side disappears from `/metrics` depending on the order, the exporter
  logs `instrument type conflict` or the gatherer an error, and `ContinueOnError` keeps the
  scrape at HTTP 200. `TestHistogramCollisionsKeepScrapesWorking` pins this.
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
  re-parses counter values, which `dogstatsd.Parse` truncates to `int64`, and sample rates,
  and rejects non-finite gauges and histogram values that are `NaN` or above 2^63 (the parser
  lets `NaN` and `+Inf` through for `|h` and `|ms`).
- client_golang recovers panics in collectors, but a panic still drops every OTel series on
  every scrape. Validate any new client-controlled data before it reaches OTel.
- `/metrics` uses `promhttp.ContinueOnError`: colliding series (e.g. `a.b` and `a_b`, or `foo`
  and `foo.total`) are logged and skipped instead of turning every scrape into an HTTP 500.

## Known quirks (not yet fixed)

- A counter and a gauge whose Prometheus names collide (`foo|c` and `foo.total|g`) make one of
  the two disappear from `/metrics`, see the protocol rules.
- Negative counter values are recorded, so a counter can go down (Prometheus reads that as
  a reset).
- A counter total or histogram `_sum` whose running total of whole values passes 2^63
  (about 9.2e18) wraps negative: the SDK adds whole-valued floats into an int64
  (`9e18|c` twice gives about -4.5e17). The per-value 2^63 bound can't prevent it. It is
  unreachable with realistic values, causes no crash and scrapes keep working.
- Each counter, gauge and histogram keeps at most 2000 tag sets (`NewPrometheusMeterProvider`).
  Beyond that, values go to a single `otel_metric_overflow="true"` series: a sum for a
  counter, the last overflowing value for a gauge, the merged observations for a histogram.
- Sample rates are not applied to histograms: a sampled `latency:12|h|@0.1` adds one
  observation, so `_count` and `_sum` undercount by 1/rate. Recording a value 1/rate times
  would be unbounded work.
- Histogram buckets are fixed: the OTel defaults are suited to milliseconds, so values in
  seconds or bytes land in a few buckets. The bound semantics themselves (a value equal to a
  bound goes in that bound's bucket) are documented behaviour, not a quirk.
- A histogram costs more than a counter. At the 2000 tag sets limit, measured with
  `BenchmarkRecordHistogramCardinality` and `BenchmarkRecordCardinality`, a histogram holds
  about 3.9 KB of heap per tag set (about 7.7 MB per name, and 18 lines per tag set on
  `/metrics`) against about 1.6 KB for a counter (about 3.1 MB per name). Names are not
  bounded, so a client can still multiply that by creating histograms with different names.
- A gauge keeps its last value on `/metrics` forever after the client stops sending it: there
  is no staleness or expiry.

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

# AGENTS.md

`quicmetrics` is a small Go proof of concept: it receives DogStatsD metrics over raw QUIC
(no HTTP/3), aggregates them with the OpenTelemetry SDK, and exposes them on a Prometheus
`/metrics` endpoint. Compose (`compose.yaml`) adds Prometheus on `:9090`.

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, `run(ctx, args, stderr) error` |
| `cmd/quicmetrics-client` | Client that generates realistic counters, gauges and histograms (`generator.go`) and pipelines them in batches |
| `internal/server` | `Server` (QUIC sessions), `Listen` + `QUICConfig` (hardened listener), `Aggregator` (counters, gauges, histograms) + `Parse`, `Limits` (`limits.go`), series expiry (`pipeline.go`, `accumulator.go`), `CertReloader`, `CAReloader`, `SelfSignedTLSConfig` |
| `internal/protocol` | `ALPN`, `DefaultAddr` and the QUIC application error codes, shared by both commands |
| `internal/tlstest` | Test-only CA and certificate issuing, so no key file is committed |

## Commands

```sh
make check                          # fmt, vet, golangci-lint v2, test -race: run before you finish
make bench                          # benchmarks, not part of check; compare runs with benchstat
make vuln                           # govulncheck
make compose-up / compose-down      # server + Prometheus in Docker
go run ./cmd/quicmetrics            # QUIC on localhost:4242/udp, metrics on :2223
go run ./cmd/quicmetrics-client -insecure  # generate all metric types for 10s (see -h for flags)
```

## Dependencies

- Go **1.27** with `github.com/quic-go/quic-go v0.64.1`: `*quic.Stream` and `*quic.Conn`
  are structs, and `quic.DialAddr` takes a `context.Context`. Never bring back
  `lucas-clemente/quic-go`.
- OTel uses the stable metric API (`go.opentelemetry.io/otel` v1.47, Prometheus exporter
  v0.69). All exporter options live in `server.NewPrometheusMeterProvider`: Prometheus-style
  names (`users.online` becomes `users_online_total`) and no `otel_scope_*` labels. Keep them
  there so tests and production expose the same series.

## Protocol rules

- Client and server take the ALPN and default address from `internal/protocol`. Never
  hardcode them. The container defaults to `-addr 0.0.0.0:4242`.
- TLS 1.3 only. The server takes `-tls-cert` and `-tls-key` (both or neither). Its
  `CertReloader` (`tls.Config.GetCertificate`) re-reads both files every `-tls-reload-interval`
  (30s by default) and switches to the new pair when their content changed, so replacing the
  files, renaming over them and the atomic `..data` symlink swap of a Kubernetes Secret all
  work. A pair that fails to load (including a cert and key read on both sides of a swap) is
  logged as a warning (once per content for parse errors, every interval for read errors), and
  the previous certificate stays. A pair that is invalid at start is a fatal startup
  error. Without the flags the server generates a self-signed P-256 certificate at every start
  and logs a warning: development only.
- mTLS: `-tls-client-ca` (PEM bundle) makes `CAReloader.RequireClientCerts` set
  `tls.RequireAndVerifyClientCert` with a `GetConfigForClient` that clones the config and
  injects the current pool per handshake, so the bundle reloads like the certificate (same
  rules: warned once per content, previous pool kept, fatal if invalid at start). Do not set
  `GetConfigForClient` elsewhere on that config. `handleConn` logs the client certificate
  subject as `client_subject`. The client takes `-tls-cert` and `-tls-key` (both or neither)
  and always presents the certificate through `GetClientCertificate`, even when the server's
  acceptable CAs don't list it, so the server makes the decision and says why.
- The client verifies the server by default: against `-tls-ca` (PEM bundle) or the system
  roots, for the host of `-addr` or `-tls-server-name`. `InsecureSkipVerify` is only set by
  `-insecure` (with a warning, and a usage error together with `-tls-ca`), so talking to a
  development server needs `-insecure`. Never default it back on.
- The server accepts **one stream per connection**: `server.QUICConfig` allows one
  bidirectional stream and no unidirectional stream, so a second stream makes `OpenStream` fail
  with `StreamLimitReachedError` on the client. The stream is a byte stream, so metrics
  are **newline-delimited**: a line ends with `\n`, a trailing `\r` is dropped, empty lines are
  skipped (and not acknowledged), and a last line without `\n` is processed when the client
  closes its side of the stream. Reads may split a line or carry several, the server never
  depends on how the client chunks its writes.
- A line is at most `server.MaxLineLength` (8 KiB) without its terminator. A longer line
  closes the session with `protocol.CodeLineTooLong` (1, `line too long`) rather than dropping
  the line, because the client is broken and the server would have to discard an unbounded stream
  to resynchronise. The close is counted as one `line_too_long` drop, which logs at most once per
  minute like the other limits. Lines acknowledged before that stay recorded.
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
- The listener comes from `server.Listen` (never `quic.ListenAddr`): it owns the UDP socket and a
  `quic.Transport` whose `VerifySourceAddress` always asks for a Retry, so every client pays one
  extra round trip and a spoofed source address gets no handshake state. `Listener.Close` only
  stops accepting (open sessions are closed cleanly by `Serve` on shutdown), and `Release` then
  frees the socket and abruptly ends what is left: call it last, after `Serve` returned.
  `QUICConfig` sets explicit idle (`-idle-timeout`, 30s) and handshake (5s) timeouts and small
  flow-control windows (64 KiB initial and 256 KiB max per stream, 128 KiB and 512 KiB per
  connection) instead of the quic-go defaults of 6 MB and 15 MB. An idle close is not logged as
  a warning.
- Application error codes live in `internal/protocol/errors.go`, one constant each in a single
  commented block, and are unique: 0 normal close, 1 line too long, 2 server busy, 3 slow client.
  Add new ones there and in the README table.
- `Server` serves at most `-max-sessions` (default 1000, `server.WithMaxSessions`) sessions at a
  time. `Serve` closes the others right after the handshake with `protocol.CodeServerBusy` (2)
  and the reason `too many sessions`, and logs a warning. Tests that call `handleConn` directly
  bypass the cap.
- A session that stops making progress is closed with `protocol.CodeSlowClient` (3), because the
  QUIC idle timeout only fires on silence and a client can keep a slot with packets that carry no
  data. `Timeouts` (`DefaultTimeouts`, `WithTimeouts`; flags `-first-stream-timeout` 10s,
  `-line-timeout` 1m, `-ack-timeout` 10s, 0 disables a deadline) has three deadlines, and the
  close reason says which one was missed: `no stream opened` (`AcceptStream` after the handshake,
  which only returns once the first bytes of the stream arrive), `no line received` (a read
  deadline on the stream, armed at the start and again each time an ack is written, so it needs a
  complete non-empty line: a partial line, however slowly it grows, and empty lines don't reset
  it) and `ack not read` (a write deadline set before each ack, which only fires once the client's
  flow-control window for acks is full: a small write returns at once while it has room).
  `serveStream` takes a `deadlineStream` so tests can run it on a `net.Pipe`. The close is counted
  as one `slow_client` drop and logged like `line_too_long`.
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
  `[A-Za-z0-9_.-/]`, up to 255 characters). Other names are dropped, counted as `invalid_name`.
  `validInstrumentName` checks this before the SDK sees the name, because the SDK registers an
  instrument in its cache before rejecting its name, which would let random invalid names pile
  up. `TestValidInstrumentNameMatchesTheSDK` pins the two together.
- Limits (`Limits`, `DefaultLimits`, `WithLimits`; flags `-max-metric-names`, `-max-tags`,
  `-max-tag-key-len`, `-max-tag-value-len`, 0 disables a check) bound what a client can make the
  server keep. Defaults: 1000 instruments, 20 tags, 64-byte keys, 200-byte values. An instrument
  is a name and a type: `foo|c`, `foo|g` and `foo|h` take three, `|h` and `|ms` share one. The
  `Aggregator` keeps the instruments it created (`instruments` in `limits.go`) and refuses new
  ones at the limit, so existing names keep recording. A metric over a limit is dropped whole,
  before it can take an instrument slot. Names starting with `quicmetrics.`, `quicmetrics_`,
  `quicmetrics-` or `quicmetrics/` are reserved (`reserved_name`) so no client can collide with the
  server's own metrics.
- Every drop is counted in `quicmetrics_metrics_dropped_total{reason}` with the reason `names`,
  `tags`, `tag_key_length`, `tag_value_length`, `reserved_name`, `invalid_name`, `line_too_long`
  or `slow_client` (one per session closed by the server, not per metric). It is an OTel counter
  on the same meter, so it shares the registry and never needs a new dependency. Logs get one
  `metrics dropped` warning per reason and minute (the first drop is logged right away) with that
  `reason`, the drops since the last one and one example cut at 64 bytes (a metric name, or the
  address and certificate subject of a client for `line_too_long` and `slow_client`): never log a
  dropped metric one by one, since the client chooses how many and what they say.
- Tag keys need an ASCII letter or digit and must not start with `__`, otherwise the whole
  metric is logged and dropped: the exporter can't turn such keys into label names. This one is
  not a limit: it warns for every metric, like the other invalid input.

## Series expiry

With `-series-ttl` (default 15m, 0 disables, `WithSeriesTTL`), a series (name and tag set) no
metric updated for the TTL disappears from `/metrics` and frees its memory. Names do not expire:
an instrument keeps its slot of `-max-metric-names` for the life of the process, as the SDK keeps
the instrument (about 700 bytes) for the life of its provider. The SDK can't forget a cumulative
series (its TODO #3006), so with a TTL:

- `NewPrometheusMeterProvider` returns a `MeterProvider` with two SDK providers: the one the
  `Aggregator` records into, whose reader has **delta** temporality (for gauges too, which
  `sdkmetric.DeltaTemporalitySelector` leaves cumulative) and only holds the series updated since
  the last scrape, and one that hosts the exporter. Without a TTL it is the plain provider.
- The `pipeline` is the exporter's `Producer`: each scrape collects that delta, adds it to the
  `accumulator` (cumulative counters and histograms, last gauge values, one state per series),
  forgets the series not seen for more than the TTL, and returns copies of the cumulative metrics.
  The exporter still does every translation, so names and options don't change.
- The accumulator applies the 2000 series limit itself, with the SDK rule (1999 series and the
  `otel.metric.overflow` one), to the series that are alive. Series that expired give their room
  back at the end of the scrape that forgets them.
- Time is only read at scrapes (`pipeline.now`): a series expires at the first scrape more than
  the TTL after the last scrape that saw an update, and nothing runs without scrapes. A long gap
  between scrapes does not expire the series that the delta shows were updated.
- An expired series that comes back restarts from zero with a new start time, like a counter that
  reset.
- The exporter keeps every metric name and type it ever exposed (`metricFamilies`), even without
  series, so an expired `foo` counter still blocks a later `foo.total` gauge, with the same outcome
  as the collision in the protocol rules.
  `TestAnExpiredCounterStillBlocksAGaugeWithTheSamePrometheusName` pins this.
- `quicmetrics_series_expired_total` counts the series forgotten, and appears with the first one.
  The `quicmetrics_metrics_dropped_total` series expire like any other.

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
  unreachable with realistic values, causes no crash and scrapes keep working. With a series TTL
  the SDK only adds up one scrape interval, and the accumulator adds float64, which wraps
  nowhere but stops counting units above 2^53.
- Invalid lines, unsupported values and invalid tag keys are still logged at warn one by one,
  with client-controlled text (quoted by `slog`). Only the drops for a limit are rate limited
  and counted.
- Each counter, gauge and histogram keeps at most 2000 tag sets (`NewPrometheusMeterProvider`),
  or 2000 unexpired ones with a series TTL. Beyond that, values go to a single
  `otel_metric_overflow="true"` series: a sum for a counter, the last overflowing value for a
  gauge, the merged observations for a histogram.
- Sample rates are not applied to histograms: a sampled `latency:12|h|@0.1` adds one
  observation, so `_count` and `_sum` undercount by 1/rate. Recording a value 1/rate times
  would be unbounded work.
- Histogram buckets are fixed: the OTel defaults are suited to milliseconds, so values in
  seconds or bytes land in a few buckets. The bound semantics themselves (a value equal to a
  bound goes in that bound's bucket) are documented behaviour, not a quirk.
- A histogram costs more than a counter. At the 2000 tag sets limit, measured with
  `BenchmarkRecordHistogramCardinality` and `BenchmarkRecordCardinality`, a histogram holds
  about 3.9 KB of heap per tag set (about 7.7 MB per name, and 18 lines per tag set on
  `/metrics`) against about 1.6 KB for a counter (about 3.1 MB per name). The names are bounded
  by `-max-metric-names`, but the product is still large: 1000 histograms that each fill their
  2000 tag sets hold about 7.7 GB, and more with long tags: those figures come from short ones,
  and 20 tags of 64 and 200 bytes cost several KB more per tag set. Lower the limit of names where
  memory is tight. There is no limit of series in all.
- A gauge keeps its last value on `/metrics` after the client stops sending it for the series
  TTL (15m by default), and forever with `-series-ttl 0`. A gauge set less often than the TTL
  therefore disappears between updates.
- With a series TTL the SDK's cardinality limit also applies to the delta of each scrape
  interval, where it cannot know the series that already exist: a client that sends 2000 new tag
  sets between two scrapes pushes the updates of established series of that instrument into
  the overflow series for that interval, so they look idle and may expire. Without a TTL
  established series keep their place. Raising the limit would only make the attack dearer.
  Likewise, an instrument full of idle series sends new ones to the overflow series until the
  scrape that expires the idle ones.
- With a series TTL, a name that stopped being sent still counts against `-max-metric-names` and
  blocks its Prometheus name for another type. Only a restart frees them.
- The TTL is counted in scrapes: an update seen by a scrape keeps the series for the TTL from
  that scrape, so a series can outlive its last update by up to one scrape interval. A metric
  recorded just before a scrape finishes may be counted in the next one, which is not lost.
- A client that sends its first bytes in time and then one complete line every `-line-timeout`,
  whether or not it reads its acks, holds a session slot for ever: the deadlines bound progress,
  not the rate. With `-max-sessions` a determined client can still fill the cap, at the price of
  one line a minute per session.
- With mTLS a rejected client is not logged by the server: `crypto/tls` has no hook for failed
  client verification. In TLS 1.3 the client only sees the rejection on its first read or
  write, not when dialing.

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
- Tests build their certificates with `internal/tlstest`. Never commit key or certificate files.
- Avoid new dependencies. Never commit `build/` or binaries.

## Review large changes

For large changes (a whole package, both commands, the protocol, or dependencies), have a
fresh agent review before you finish. Give it the goal, `git diff`, and this file, but not
your reasoning. Fix what it finds, re-run `make check`, and report the outcome to the user.

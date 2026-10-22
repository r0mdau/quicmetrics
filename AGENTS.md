# AGENTS.md

`quicmetrics` is a small Go proof of concept: it receives DogStatsD metrics over raw QUIC
(no HTTP/3), aggregates them with the OpenTelemetry SDK, and exposes them on a Prometheus
`/metrics` endpoint. Compose (`compose.yaml`) adds Prometheus on `:9090`.

| Path | Role |
| --- | --- |
| `cmd/quicmetrics` | Server command: flags, wiring, `run(ctx, args, stderr) error` |
| `cmd/quicmetrics-client` | Sample client |
| `internal/server` | `Server` (QUIC sessions), `Aggregator` + `Parse`, `SelfSignedTLSConfig` |
| `internal/protocol` | `ALPN` and `DefaultAddr`, shared by both commands |

## Commands

```sh
make check                          # fmt, vet, golangci-lint v2, test -race: run before you finish
make vuln                           # govulncheck
make compose-up / compose-down      # server + Prometheus in Docker
go run ./cmd/quicmetrics            # QUIC on localhost:4242/udp, metrics on :2223
go run ./cmd/quicmetrics-client     # send sample counters
```

## Pinned dependencies

- Go **1.26** with `github.com/quic-go/quic-go v0.64.1`: `*quic.Stream` and `*quic.Conn`
  are structs, and `quic.DialAddr` takes a `context.Context`. Never bring back
  `lucas-clemente/quic-go`.
- OTel metrics are **pre-1.0 (`v0.33.0`)**, and the code relies on APIs removed later
  (`meter.SyncFloat64()`, the `metric/instrument` package). Don't bump OTel as a side effect.

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

## Crash safety

One client must never take the server down.

- `panic` and fatal exits are only allowed at startup, in `cmd/`. Per-metric errors are
  logged and the metric is dropped. `handleConn` recovers panics within its own session.
- Always parse with `server.Parse`, never `dogstatsd.Parse`. It recovers the parser's panic on
  empty segments (`foo:1|c|`) and rejects invalid UTF-8.
- Panics while serving `/metrics` **can't be recovered**: they happen in client_golang
  goroutines. The exporter panics on names that aren't valid UTF-8, so validate any new
  client-controlled data before it reaches OTel.
- `/metrics` uses `promhttp.ContinueOnError`: colliding series (e.g. `a.b` and `a_b`) are
  logged and skipped instead of turning every scrape into an HTTP 500.

## Known quirks (not yet fixed)

- Counter values are truncated to `int64` (`1.5` is recorded as `1`). Sample rates are ignored.
- The exporter appends `_total` once per data point, so you get `x_total`, `x_total_total`,
  and so on. Fixing it requires an OTel upgrade.

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
- Avoid new dependencies. Never commit `build/` or binaries.

## Review large changes

For large changes (a whole package, both commands, the protocol, or dependencies), have a
fresh agent review before you finish. Give it the goal, `git diff`, and this file, but not
your reasoning. Fix what it finds, re-run `make check`, and report the outcome to the user.

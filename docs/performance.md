# Performance data collection

This opt-in suite runs and emits performance data. The HTTP load generator,
collector and synthetic tests are written in Go and live in `e2e/`. No external
load-testing executable or script is required. There are no latency thresholds,
regression gates, production optimizations or automatic CI load jobs.

Normal `go test ./...` does not run benchmarks or live performance tests.
Defaults are short exploratory runs; small sample sets do not provide reliable
tail estimates. Percentiles are descriptive, not performance conclusions.

## Runtime-free regression checks

CI runs the existing synthetic collector, lifecycle, stream and report tests
both normally and with the race detector. These use fixtures/local HTTP servers,
not a sandbox runtime, live load collection or latency thresholds:

```sh
synthetic_tests='^Test(Performance.+|HTTPPerformance.+|Lifecycle(Measurements|Verification).+|ReadFirstLogContent.+|FirstLogReadCancellation.+|WritePerformanceReport.+)$'
go test -tags='e2e performance' ./e2e -run "$synthetic_tests" -count=1
go test -race -tags='e2e performance' ./e2e -run "$synthetic_tests" -count=1
```

The selector includes lifecycle verification-error accounting and excludes the
live `TestPerformance` and `TestEndToEnd` entry points. Keep the families in sync
when adding synthetic tests. Tagged vet also includes `performance` code.

## Runtime-free benchmarks

From the repository root:

```sh
go test ./internal/api -run '^$' -bench '^BenchmarkAuthenticatedAPI' -benchmem -count=1
go test ./internal/database -run '^$' -bench '^BenchmarkSQLiteRepository' -benchmem -count=1
```

Go prints `ns/op`, `B/op` and `allocs/op`. These benchmarks do not generate the
live JSON report; avoid race instrumentation when collecting timing data.

- [API benchmarks](../internal/api/performance_bench_test.go): authenticated
  list, inspect and command history, with 1/100/1000 list/history entries. A
  fixture returns prebuilt domain values. Timed work includes routing, auth,
  DTO/JSON handling and a new `httptest.ResponseRecorder`; requests are prepared
  before timing. No network, database, real runtime or CLI execution is measured.
- [SQLite benchmarks](../internal/database/performance_bench_test.go): list,
  lookup and command history with public/native views and 1/100/1000 sandbox
  rows and commands. All commands belong to one sandbox. Memory/file modes use
  one connection. Setup, migration, seeding, validation and cleanup are outside
  timing. Native views use native IDs. These are repeated reads, not writes,
  durability, contention or cold-disk measurements. File mode benefits from
  SQLite/OS caches; memory mode excludes persistent-storage costs.

## Live prerequisites

Use the project's Go toolchain, `curl` with `.localhost` support and an already
running local runtime. Docker requires its CLI and local daemon; CLI and server
must use the same `DOCKER_HOST` if set. Remote daemons are unsupported.
Apple requires Apple Silicon macOS, Apple Container 1.4.1, a configured kernel
and locally available trusted vminit. See [runtime setup](runtimes.md).

Registry access to `node:25-alpine` is needed. Each run pulls, exports and imports
the selected OCI image into private state, warms the native image cache and
completes an untimed lifecycle. Build/startup, image preparation, warm-up and
cleanup are excluded from measurement. Runtime/registry problems can fail setup.

## Run Docker, then Apple

Run from the repository root, sequentially. Do not concurrently run tools that
mutate the same runtime resources. The harness supplies its own API key.

```sh
(
  set -eu
  reports="$HOME/opensbx-performance-artifacts"
  mkdir -p "$reports/docker" "$reports/container"

  OPENSBX_E2E_RUNTIME=docker \
  OPENSBX_PERF_ARTIFACTS="$reports/docker" \
  go test -tags='e2e performance' ./e2e \
    -run '^TestPerformance$' -count=1 -v -timeout=15m

  OPENSBX_E2E_RUNTIME=container \
  OPENSBX_PERF_ARTIFACTS="$reports/container" \
  go test -tags='e2e performance' ./e2e \
    -run '^TestPerformance$' -count=1 -v -timeout=15m
)
```

The subshell stops if Docker fails. Omit the second invocation on a Docker-only
host. `-count=1` avoids cached results; the exact selector excludes ordinary E2E
scenarios and synthetic tests. Do not add `-race` to timing runs. The suite
timeout accommodates setup and cleanup; larger settings may require more time.
Let deadlines and cleanup finish instead of killing the test runner.

## Configuration

Unset or empty values use the following defaults. Bounds are inclusive and
validated before live setup.

| Variable | Default | Meaning / bounds |
| --- | --- | --- |
| `OPENSBX_E2E_RUNTIME` | `docker` | `docker` or `container` |
| `OPENSBX_PERF_ITERATIONS` | `3` | 1–20 lifecycle and exec/log samples |
| `OPENSBX_PERF_COMMAND_HISTORY` | `10` | 1–100 preloaded commands, plus proxy and measured commands |
| `OPENSBX_PERF_RATE` | `5` | 1–20 total mixed arrivals/second, not per endpoint |
| `OPENSBX_PERF_DURATION` | `10s` | 1s–2m arrival scheduling window |
| `OPENSBX_PERF_MAX_VUS` | `10` | 1–50 concurrent requests; name retained for existing configurations |
| `OPENSBX_PERF_REQUEST_TIMEOUT` | `5s` | 1s–30s HTTP-load and first-log-data deadline |
| `OPENSBX_PERF_ARTIFACTS` | Retained temporary directory | Absolute directory, printed in verbose output |
| `OPENSBX_E2E_BINARY` | Build `cmd/api` | Existing executable; prefer an absolute path |
| `OPENSBX_E2E_ARTIFACTS` | No retained process log | Optional absolute diagnostics directory |

Go duration strings including `1.5s` and `1500ms` work within these bounds.
`OPENSBX_PERF_PREALLOCATED_VUS` and `OPENSBX_PERF_K6` are obsolete and unused;
the Go generator has no virtual-user preallocation or executable prerequisite.
The shared harness retains its own 90-second HTTP deadline; command completion
observation uses 15 seconds.

## HTTP load model

[The Go generator](../e2e/performance_load_test.go) schedules one GET per arrival,
round-robin across five routes:

| Report endpoint | Request |
| --- | --- |
| `health` | `/v1/health`; includes actual runtime `Ping` |
| `sandbox_list` | `/v1/sandboxes`; validates the single test sandbox |
| `sandbox_inspect` | `/v1/sandboxes/{id}` |
| `command_history` | `/v1/sandboxes/{id}/cmd`; validates the fixed history count |
| `proxy_app` | Test-owned `.localhost` URL `/`, controlled Node app |

Arrival deadlines are absolute and independent of response time. There is no
request queue: arrivals are dropped if all concurrency slots are busy. If the
scheduler misses an entire arrival interval, that arrival is dropped rather than
sent in a catch-up burst. The report separates capacity drops, scheduler drops
and future arrivals canceled by the parent context;
`offered = achieved + dropped` always reconciles. Achieved counts completed
attempts including failures, not only successful responses. Achieved rate is
completed attempts divided by the configured scheduling window; some complete
after that window. Total elapsed load time includes draining pending requests.

Pending requests finish under their own deadline; ending the arrival window does
not cancel them. The collector's outer deadline is duration + request timeout +
2s. Parent cancellation joins all requests and fails collection with partial
data retained. There is no latency gate for slow responses or dropped arrivals.

The transport reuses connections, bypasses ambient proxies, refuses redirects
and dials only configured loopback destinations. Sandbox `.localhost` URLs
connect to `127.0.0.1` while retaining the original HTTP Host. This is not an OS
DNS/browser compatibility test. Authentication is sent only to management routes,
never health or the sandbox app. This transport differs from the CLI client,
which disables keepalive to prevent implicit mutation replay.

Per-route data includes requests, validated successes, status/payload failures,
transport errors and timeouts (a subset of transport errors). Every started
attempt has a latency sample, including errors. Timing uses Go's monotonic
`time.Since`, from HTTP request dispatch through complete bounded body reading
and closing, including connection establishment but excluding payload validation.
Reports store fractional
milliseconds; no fixed clock-resolution guarantee is claimed. An unvisited route
has known-zero counts and `latency_ms: null`. Dropped counts are exact observed
counts, including zero.

## Lifecycle, commands and resources

The fixture has one sandbox during load and at most two during lifecycle
measurement. Both runtimes request 1 CPU, but Docker requests 128 MiB and Apple
1024 MiB to accommodate guest startup. These are not equivalent configurations
for ranking runtime performance.

- `lifecycle/create`, `stop`, `start`, `delete`: local HTTP timings including
  encoding, transfer and body reading. Ownership/native verification is untimed.
  If verification fails after a timed response, its diagnostic does not mark the
  already collected sample missing. Uncertain state stops subsequent creations
  and dependent measurement phases; exact-resource cleanup still runs.
- `exec/acceptance`: POST through command registration, excluding guest completion.
- `exec/completion`: POST start until the API exposes guest exit code; includes
  25ms polling and the Node workload's intentional 150ms dwell.
- `logs/first_data`: attachment start to first nonempty stdout/stderr NDJSON
  record. Empty records/headers are not data. A split marker is validated across
  records without moving the first-data timestamp. Output may already be retained;
  this is not guest emit-to-consumer latency.
- `http/load`: scheduling window plus draining, not individual endpoint latency.

One-second macOS/Linux `ps` samples observe only the OpenSBX server PID: CPU
percentage and RSS in kilobytes. They exclude runtime daemons, guests and the Go
test/load-generator process. Linux `%cpu` is process-lifetime average, not an
instantaneous one-second delta; platform semantics differ. Unsupported samples
are unavailable, not invented zeros.

## Reports and cleanup

Verbose output prints the summary and a unique private JSON file:
`opensbx-performance-<runtime>-<UTC timestamp>-<pid>.json`. There is only one report
per run, with schema version **2**, an `http` section and `http/` measurement names.
The previous schema's tool-specific fields and companion files are removed.
The timing method changed, so older integer-millisecond reports should not be
treated as equivalent baselines without accounting for the different collector.

Reports include runtime/image digest, OS/architecture/Go, revision/dirty state,
generator/connection policy, configuration, dataset, counts, scopes, diagnostics,
small-sample warnings, process observations and `test_failed`. Outcome counters,
arrival counts and latency counts are reconciled before accepting load results.
JSON `errors` and verbose `error_records` are diagnostic records; use per-endpoint
counters for request-error counts, without double-counting timeouts.

Reports refuse overwrite and use private permissions. API keys and raw error
details are not serialized. Optional diagnostics retain the shared harness's
runtime-server log; review those before sharing and avoid secrets in paths.

Collection fails for invalid configuration, setup, broken measurement integrity,
report writing, ownership verification or cleanup. Measured request errors,
timeouts and dropped load are data; a passing test is not a performance/error-rate
endorsement. A report is attempted after report setup even on subsequent failure;
early setup errors or externally killing the runner may leave no report.

[Harness cleanup](../e2e/README.md#isolation-and-cleanup) removes only exact owned
resources, verifies native/database/cache absence and removes disposable state.
Existing resources/cache references are preserved. No global prune or runtime
service stop is performed. Ambiguous Docker image ownership is rejected before
materialization. Cleanup failures remain failures.

Sources: [collector](../e2e/performance_test.go),
[report/configuration helpers](../e2e/performance_support_test.go),
[helper regressions](../e2e/performance_support_test_cases_test.go),
[synthetic HTTP tests](../e2e/performance_load_cases_test.go).

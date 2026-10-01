# Contributing

## Build and run

Use Go **1.25.13 or newer**. A local runtime is required to run the server, but not
for the default unit tests.

```sh
go test ./...
go test -race ./...
go vet ./...
go vet -tags='integration,appleintegration,e2e,performance' ./...
go build ./...
git diff --check
```

Run the pinned analyzers with the CI toolchain, even if a newer Go is installed
locally (analyzer export-data support can lag behind new Go releases):

```sh
GOTOOLCHAIN=go1.25.13 go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
GOTOOLCHAIN=go1.25.13 go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
```

The reusable `.github/workflows/verify.yml` is called for PRs, main, manual runs,
weekly scheduled checks, and releases. Every job checks out the supplied exact
commit SHA and uses the Go version in `go.mod`. It defines:

- Hermetic unit and race tests on native Linux, macOS and Windows, with five-minute
  package timeouts, coverage profiles and per-package/function coverage reports.
  Coverage is evidence, not an arbitrary 100% gate.
- Default, race and integration/E2E/performance-tagged vet, pinned Staticcheck
  `v0.7.0` and govulncheck `v1.1.4`, and runtime-free performance regressions
  (normal and race). No scheduled live load/performance collection is run.
- Repository-wide gofmt debt reporting, with enforcement on Go files changed by
  the PR (or current commit for other runs), plus whitespace diff checks. Existing
  formatting debt is not rewritten as a side effect.
- Mandatory Docker integration with `OPENSBX_REQUIRE_DOCKER_INTEGRATION=1` and
  an explicit bounded `docker info` prerequisite. Local optional runs may skip
  without Docker; mandatory runs must fail rather than skip. The prepared-store
  cross-runtime test remains separately opt-in.
- Short fuzz smoke in `internal/proxy`, `internal/client`, `internal/terminaltext`
  and `internal/sandbox`: every package must expose at least one `Fuzz*` target;
  zero targets fails the job. Targets are discovered, then run individually for
  10s (60s on schedule). Scheduled checks also shuffle and repeat sensitive
  runtime/client/proxy/domain package tests under race instrumentation.
- [Real-process E2E](e2e/README.md) on Docker Linux amd64 and Apple `macos-26`
  arm64. Each native job runs the GoReleaser-produced **CGO-disabled package**
  first, then a complementary daemon built with
  `CGO_ENABLED=1 go build -race ./cmd/api`. `OPENSBX_E2E_BINARY` selects the
  executable. These runs are sequential, never simultaneous on one runtime.

Go test commands retain structured JSON output, command failures propagate
through `pipefail`, and diagnostics are uploaded even on failure. Job timeouts
bound infrastructure stalls. Only obsolete PR executions cancel one another;
release executions do not cancel each other.

`CI Gate` explicitly requires successful reusable verification. Its internal
`Verification Gate` requires all mandatory jobs to succeed; skipped, canceled or
failed jobs do not count as success. **Repository maintainers must configure the
GitHub branch ruleset to require `CI Gate`**; workflow files alone cannot enforce
merge protection. This change does not configure GitHub settings.

Test support honors Docker strict mode and rejects child-process race reports,
including reports from intentionally killed or restarted daemons. The E2E harness
explicitly sets `GORACE=halt_on_error=1 exitcode=66` on daemon children and scans
all child logs for `WARNING: DATA RACE`, even when a scenario expects a nonzero
child exit. The four required packages provide fuzz targets. These assertions
remain complementary to the race-daemon build; local hermetic checks and workflow
definition alone are not evidence that hosted native CI or live E2E have passed.

### Security baseline

Verification and publishing both select the exact Go version from `go.mod`.
The minimum is **1.25.13**, a patch-only toolchain update covering the reachable
standard-library findings identified with govulncheck:

- GO-2026-4337, GO-2026-4601, GO-2026-4602, GO-2026-4603,
  GO-2026-4865, GO-2026-4869, GO-2026-4870, GO-2026-4918,
  GO-2026-4946, GO-2026-4947, GO-2026-4971, GO-2026-4976,
  GO-2026-4977, GO-2026-4980, GO-2026-4982, GO-2026-4986,
  GO-2026-5026, GO-2026-5037, GO-2026-5039, GO-2026-5856,
  GO-2026-5972, GO-2026-6089, GO-2026-6090, GO-2026-6091,
  GO-2026-6218.

The dependency patches address:

| Finding | Minimum fix | Selected version |
| --- | --- | --- |
| [GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970), invalid-UTF-8 normalization infinite loop | `golang.org/x/text v0.39.0` | `v0.39.0` |
| [GO-2026-5026](https://pkg.go.dev/vuln/GO-2026-5026), invalid ASCII-only Punycode hostname acceptance | `golang.org/x/net v0.55.0` and Go `1.25.13` | `v0.56.0` and Go `1.25.13` |
| [GO-2026-5676](https://pkg.go.dev/vuln/GO-2026-5676), HTTP/3 QPACK trailer memory exhaustion | `github.com/quic-go/quic-go v0.59.1` | `v0.59.1` |

`x/text v0.39.0` requires `x/tools v0.47.0`, which requires `x/net v0.56.0`;
the selected versions are the minimum compatible graph, not a blanket update.
That graph also requires `x/crypto v0.53.0`, `x/mod v0.37.0`,
`x/sync v0.21.0` and `x/sys v0.46.0`. These retain Go 1.25 compatibility.
The pinned govulncheck scans reachable symbols; a passing scan is not a claim
that no advisory exists anywhere in the dependency graph. Re-run it as the
vulnerability database changes.

The local Go 1.25.13 scan after these patches reports **zero reachable findings**.
Its verbose output still lists imported-package finding GO-2026-5158 (`otel`)
and module-only findings GO-2026-6355, GO-2026-6354, GO-2026-6303,
GO-2026-5932 (`x/crypto`), GO-2026-6180, GO-2026-6179 (`x/mod`),
and GO-2026-5841 (`compress/s2`). The scanner found no calls to the affected
symbols; these were not suppressed or used to justify unrelated upgrades.

Run the live Docker integration suite:

```sh
go test -tags=integration ./... -run '^TestIntegration'
```

Apple live testing requires Apple Silicon macOS, a running Apple container 1.4.1
service, a configured kernel and locally available vminit. See [runtime setup](docs/runtimes.md).
Prepare the workload image in an isolated OpenSBX data directory, then opt in:

```sh
OPENSBX_APPLE_INTEGRATION=1 \
OPENSBX_APPLE_TEST_IMAGE=node:25-alpine \
OPENSBX_APPLE_TEST_DATA_DIR=/absolute/path/to/prepared-data \
go test -tags=appleintegration ./internal/applecontainer \
  -run '^TestAppleRuntimeEndToEnd$' -count=1 -v
```

The prepared-store Docker/Apple portability test is also opt-in. It requires both
runtimes running and a prepared `linux/arm64` `node:25-alpine` image in the
specified isolated store:

```sh
OPENSBX_CROSS_RUNTIME_INTEGRATION=1 \
OPENSBX_CROSS_RUNTIME_DATA_DIR=/absolute/path/to/prepared-data \
go test -tags='integration,appleintegration' ./internal/api \
  -run '^TestIntegration_ManagedOCIImagePortability$' -count=1 -v
```

Live tests use real local resources; use a dedicated test data directory and do
not clean up resources globally.

## Real-process end-to-end tests

The E2E suite starts the actual executable, exercises REST and MCP over HTTP
against a real runtime, visits returned sandbox domains, and verifies runtime
resource cleanup and empty database tables before removing its temporary
database/catalog directory. CI passes the binary extracted from a GoReleaser
snapshot archive, then the separately race-instrumented daemon; locally it builds
`cmd/api` unless `OPENSBX_E2E_BINARY` names an executable to test. `go test -race`
instruments the harness, **not** a separately packaged child executable:

```sh
go test -tags=e2e ./e2e -run '^TestEndToEnd$' -count=1 -v -timeout=12m
OPENSBX_E2E_RUNTIME=container go test -tags=e2e ./e2e -run '^TestEndToEnd$' -count=1 -v -timeout=12m
```

See [E2E prerequisites, scenarios and cleanup guarantees](e2e/README.md). Apple
requires its native runtime prerequisites; neither local command installs a
runtime. The CI Apple job uses the `macos-26` arm64 runner, installs signed Apple
Container 1.4.1, prepares the recommended kernel and vminit, and stops the
ephemeral runtime afterward.

## Performance data collection

See [performance commands, prerequisites and measurement scopes](docs/performance.md)
for runtime-free benchmarks and opt-in Docker/Apple collection. These emit data
only: no latency thresholds, regression gates, or automatic CI load job. Live
timing runs use the Go load generator in `e2e` and run sequentially without race
instrumentation.

## API documentation

Swagger is generated from annotations in the API handlers. When changing those
annotations, regenerate the checked-in artifacts with the pinned generator:

```sh
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
  -g ./cmd/api/main.go -o docs --parseDependency --parseInternal
```

The generated `docs.go`, `swagger.json` and `swagger.yaml` are served at
`/swagger/index.html` when the server is running.

## Releases

Release maintainers create and push a version tag (for example, `v1.2.0`). The
`release.yml` workflow first calls the same reusable verification on the exact
tag commit SHA. Publication depends on successful verification (including both
native E2E variants); it does not trust an unrelated earlier main-branch run.
Only the publishing job has `contents: write`; verification has read permission.
GoReleaser `v2.14.1` builds and publishes binaries and checksums to GitHub Releases;
packaging configuration is in `.goreleaser.yml`. No circular workflow dependency
or release cancellation group is used.

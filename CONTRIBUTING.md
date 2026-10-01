# Contributing

## Build and run

Use Go **1.25.6 or newer**. A local runtime is required to run the server, but not
for the default unit tests.

```sh
go test ./...
go test -race ./...
go vet ./...
go vet -tags='integration,appleintegration,e2e' ./...
go build ./...
git diff --check
```

CI runs unit tests, race tests, vet for default and integration/E2E builds, the
Docker-tagged integration suite, and the [real-process E2E suite](e2e/README.md)
on Docker and Apple Container. The E2E jobs exercise the GoReleaser-produced
release archive on native Linux amd64 and macOS arm64 runners. The older
integration tests skip locally if Docker is unavailable; the E2E suite fails
instead. Run the live Docker integration suite:

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
snapshot archive; locally it builds `cmd/api` unless `OPENSBX_E2E_BINARY` names an
executable to test:

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
`release.yml` workflow builds and publishes binaries and checksums to GitHub
Releases. The project uses GoReleaser; packaging configuration is in
`.goreleaser.yml`.

# Testing guide

## Default suite and release checks

Use Go **1.25.6 or newer**. The default suite uses hermetic runtime/registry
fixtures and does not require a running sandbox runtime. Tests that need a real
local Docker daemon skip when it is unavailable; a skip is not a live pass.

```sh
go test ./... -count=1 -coverprofile=coverage.out
go tool cover -func=coverage.out
go test -race ./...
go vet ./...
go build ./...
git diff --check
```

The suite exercises typed domain contracts, DTO conversions, public/native
identity separation, ownership recovery, OCI integrity and publication,
concurrency, registry URL policies, local Host/Origin isolation, and native
adapter command/file/lifecycle behavior. Hermetic adapter checks do not substitute
for testing an actual daemon.

For the Apple ownership-persistence failure path alone:

```sh
go test ./internal/applecontainer \
  -run '^TestCreateOwnershipFailureAndRollbackFailureAttemptRecoveryPersistence$' \
  -count=1
```

## Apple live integration

Requirements: Apple Silicon macOS, already-running Apple container CLI/server
**1.4.1**, a locally configured kernel, and the configured trusted vminit image
available locally. See [Apple prerequisites](apple-container.md). Preparing a
workload image alone does not prepare the runtime's init filesystem.

With the validated default configuration, vminit can be prepared explicitly:

```sh
container image pull --platform linux/arm64 --progress plain \
  ghcr.io/apple/containerization/vminit:0.45.0
```

This is an intentional runtime dependency download, not an OpenSBX workload
catalog operation. Alternatively, load a trusted OCI archive under the configured
reference. Kernel installation/configuration is also an explicit runtime setup
step; it is never performed implicitly by OpenSBX. Keep the zero-download guard
enabled during sandbox creation.

Choose an isolated OpenSBX data directory containing the prepared `linux/arm64`
workload, then run:

```sh
OPENSBX_APPLE_INTEGRATION=1 \
OPENSBX_APPLE_TEST_IMAGE=node:25-alpine \
OPENSBX_APPLE_TEST_DATA_DIR=/absolute/path/to/prepared-isolated-data \
go test -tags=appleintegration ./internal/applecontainer \
  -run '^TestAppleRuntimeEndToEnd$' -count=1 -v
```

The data-directory variable is optional: without it, the test creates a temporary
store. If the named artifact is missing, the opt-in test explicitly pulls it into
that store. Supplying the exact prepared platform avoids this download. The test
does not install/start services or automatically provision kernel/vminit.

The test uses a fresh execution DB and generated identities, cleaning up only its
own sandboxes and exact workload cache reference. Use a dedicated environment;
do not externally mutate those resources during the run. Runtime infrastructure
images are distinct from managed workload images and are not removed as workload
cleanup. Never use global prune or service stop for test cleanup.

## Docker live integration

With a local Docker daemon available:

```sh
go test -tags=integration ./... -run '^TestIntegration' -count=1
```

These tests use `node:25-alpine`, isolated OpenSBX stores and owned sandboxes.
They may explicitly pull the workload artifact into the catalog. Existing native
images are not automatically adopted. The tests skip when Docker is unavailable.

## Last recorded verification and bounds

- The last complete default, race and vet runs passed. Aggregate statement
  coverage was **90.0042% (4259/4732)**, before the final Apple ownership-recovery
  correction. That correction's transient and persistent failure regression now
  passes; the full suite, race run and coverage measurement have not been repeated
  for that small patch. Coverage is a revision-specific measurement, not a release
  guarantee for subsequent changes.
- The Apple **1.4.1** live smoke passed in **36.24 seconds** after explicit vminit
  preparation. It exercised source OCI -> verified native cache -> sandbox,
  sibling-isolated SIGTERM/SIGKILL, files, TTL/lifecycle, same-port friendly-host
  versus control routing, and native cache-loss reimport without additional
  manager registry requests.
- The configured vminit was retained as runtime infrastructure after cleanup:
  service image count **1**, ordinary visible image list **[]**. It is not an
  OpenSBX workload catalog entry.
- Docker live verification was unavailable because the local daemon socket did
  not exist. Passing hermetic Docker adapter checks is not a cross-runtime live
  certification.
- Actual HTTP routing was verified. These HTTP-level results do not certify
  real-browser compatibility or interactive browser workflows.

## CI

The current workflow runs default tests on pushes to `main` and pull requests,
then invokes the Docker-tagged integration suite. Apple live integration remains
an explicit opt-in command outside that workflow.

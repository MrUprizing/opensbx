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
go vet -tags='integration,appleintegration' ./...
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
go test -tags=integration ./internal/api -run '^TestIntegration_' -count=1 -v
```

These tests use `node:25-alpine`, isolated OpenSBX stores and owned sandboxes.
They may explicitly pull the workload artifact into the catalog. Existing native
images are not automatically adopted. The tests skip when Docker is unavailable.

The immediate-kill regression starts `sleep 3600` asynchronously and sends SIGTERM
immediately after the command response, for ten cycles. Each kill and terminal
wait request has a **five-second deadline**; the test requires a nonzero terminal
exit code, not just a successful signal request. Run it alone with:

```sh
go test -tags=integration ./internal/api \
  -run '^TestIntegration_ImmediateKillWaitsForExecStartupAndCompletes$' \
  -count=1 -v -timeout=120s
```

Docker command signaling requires guest `pkill` with extended-regex and full
command-line matching support. It waits for the original native exec to report
both Running and a positive PID, then passes an escaped, full-match argv pattern
directly to `pkill`, without shell interpolation. The native PID is only a startup
acknowledgment, not a guest signal target. Selection is still by command-line text:
identical space-joined argv in the same sandbox can match multiple processes; this
is not a per-command PID-isolation guarantee.

## Prepared-store Docker/Apple portability

This test requires Apple Silicon macOS, both local runtimes already running with
native `linux/arm64` support, and the Apple kernel/vminit prerequisites above. Use
an isolated OpenSBX data directory whose catalog already contains a complete
prepared `linux/arm64` variant of **`node:25-alpine`**. The guest needs Node.js for
the HTTP routing check, plus the shell/file utilities and `pkill` used by the
adapters. Unlike the Docker-only suite, this test rejects and counts manager
registry requests: it does not pull a missing workload or install runtime
dependencies. Prepare the store explicitly beforehand using the
[image CLI](images.md#runtime-independent-cli).

Always set the prepared directory explicitly:

```sh
OPENSBX_CROSS_RUNTIME_INTEGRATION=1 \
OPENSBX_CROSS_RUNTIME_DATA_DIR=/absolute/path/to/prepared-isolated-data \
go test -tags='integration,appleintegration' ./internal/api \
  -run '^TestIntegration_ManagedOCIImagePortability$' -count=1 -v
```

Run the whole test for the parity comparison, not just one runtime subtest. It
checks shared root/selected-manifest identity, distinct public/native sandbox IDs,
files, stdout/stderr, exit status, Docker's generated `<Name>.localhost` routing
(including `/v1/health` remaining an app path), and native cache-loss recovery
without registry requests.

Each runtime uses a fresh execution DB. Cleanup removes only recorded test-owned
sandboxes and exact private cache references introduced by the test. If a matching
cache existed beforehand, it is preserved and that runtime's cache-deletion/reimport
case is not covered; read the ownership/recovery logs before claiming coverage.
Use isolated resources and avoid concurrent tests or external mutations. Preserve
runtime infrastructure such as vminit; never prune globally, remove unrelated
images, switch daemon storage settings, or stop runtime services for cleanup.

## Last recorded verification and bounds

Recorded **2026-09-27**, after the Docker native-cache identity, cancelable cache
locks, exec-start readiness and literal command-signaling corrections:

- The uncached default suite passed with aggregate statement coverage
  **90.0161% (4463/4958)**. The full race suite, default and integration-tagged vet,
  and diff check passed; final code and security reviews were approved. Coverage
  is a revision-specific measurement, not a guarantee for subsequent changes.
- All five Docker API live tests passed on **Docker 29.4.3**, Linux `aarch64`,
  using the **`io.containerd.snapshotter.v1`** image store:

  | Test (`TestIntegration_` prefix) | Duration | Result |
  | --- | --- | --- |
  | `FullLifecycle` | 13.81 s | PASS |
  | `ImmediateKillWaitsForExecStartupAndCompletes` | 8.95 s | PASS, ten bounded cycles |
  | `NotFound` | 0.64 s | PASS |
  | `DefaultResourceLimits` | 8.41 s | PASS |
  | `ImagePull` | 5.52 s | PASS |

- `TestIntegration_ManagedOCIImagePortability` passed in **17.73 s**:
  Docker **6.24 s**, actual Apple Container **1.4.1** **10.19 s**. Both used the
  same prepared artifact:
  - OCI root: `sha256:bdf2cca6fe3dabd014ea60163eca3f0f7015fbd5c7ee1b0e9ccb4ced6eb02ef4`
  - Selected manifest: `sha256:818761312e481165909adaaec0a09e23076e2fed626000c8e286f1d9470e4551`
  - **Zero manager registry requests**, including actual cache deletion and
    offline reimport on both runtimes.
  - Matching file contents, separate stdout/stderr and exit **7**, distinct
    public/native IDs, and successful Docker Node HTTP/Host routing and signaling.
- Docker's **classic image store is covered hermetically only**. The real daemon
  used containerd throughout; no global storage configuration was switched.
- Cleanup preserved pre-existing Docker resources and retained Apple runtime
  infrastructure, leaving no test-owned sandboxes. No global runtime configuration
  changes were needed.
- These are real-runtime and HTTP-level results, **not browser QA**. Browser
  compatibility and interactive workflows remain outside this verification.

## CI

The current workflow runs default tests, race tests, and default and
integration-tagged vet on pushes to `main` and pull requests, then invokes the
Docker-tagged integration suite. Apple live integration remains
an explicit opt-in command outside that workflow.

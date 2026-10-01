# Real-process end-to-end suite

This suite compiles `cmd/api`, launches the resulting binary as a separate
process, and drives its public REST and MCP interfaces over a real loopback
listener. Runtime operations use actual Docker containers or Apple containers.
The `e2e` build tag keeps these tests out of `go test ./...`.

CI packages the release archive with GoReleaser, extracts the native artifact,
and runs this suite against that exact executable with `-race`. Both Docker and
Apple Container jobs are required; the Apple job provisions its ephemeral
[GitHub-hosted macOS 26 arm64 runner](https://github.com/actions/runner-images)
with the pinned signed runtime and kernel prerequisites.
When `OPENSBX_E2E_BINARY` is unset, local runs build `cmd/api` directly.

## Run

Docker (also mandatory in CI):

```sh
go test -tags=e2e ./e2e -run '^TestEndToEnd$' -count=1 -v -timeout=12m
```

Apple Silicon macOS with Apple container 1.4.1 already running, its configured
kernel installed and trusted vminit image locally available:

```sh
OPENSBX_E2E_RUNTIME=container \
go test -tags=e2e ./e2e -run '^TestEndToEnd$' -count=1 -v -timeout=12m
```

Requirements: the project's Go toolchain, runtime CLI, running local runtime,
`curl` with `.localhost` support, and registry access to pull `node:25-alpine`.
The suite never silently skips a missing prerequisite. Use the same local Docker
endpoint for the CLI and server (`DOCKER_HOST` when needed); remote daemons are
unsupported. Run the whole suite: the named subtests deliberately share the
prepared image, and later phases depend on earlier phases succeeding.

Use a dedicated runtime environment and do not run this suite concurrently with
another test or tool changing its resources. Existing native cache references are
preserved. If Docker already has the workload config without its exact OpenSBX
cache tag, the suite rejects that ambiguous ownership case before creating a
sandbox: deleting the new tag could otherwise also remove an existing untagged
image. Use an isolated Docker daemon for that case.

## Performance data collection

The separate `TestPerformance` requires both `e2e` and `performance` tags and uses
a native Go HTTP load generator. See [performance documentation](../docs/performance.md)
for exact commands, bounded settings, report interpretation and sequential
Docker/Apple runs without race instrumentation. It emits data only, without
latency thresholds or an automatic CI load job; ordinary E2E runs do not execute it.

## Scenarios

| Subtest | Assertions |
| --- | --- |
| AuthenticationAndOrigins | Health/readiness, valid API key, missing/wrong keys rejected on REST and MCP, foreign Origin rejected |
| ImagesPullExportImport | Missing image rejected, explicit pull/list/inspect, CLI export/remove/import/inspect, selected OCI manifest preserved |
| CLITransportWorkflow | Real binary CLI authentication/create/exec/file operations, empty and large Unicode text, guest exit status, detached kill/wait, a >10-second silent `logs --follow`, API/MCP visibility of the CLI-owned resource, and two identical-argv commands where CLI signaling must terminate only the selected command |
| SandboxCommandsFilesDomainAndLifecycle | Actual running resource and public/native identities, inventory/network/stats, stdout/stderr/exit code, log streaming, kill, file CRUD, app URL, stop/start/restart and file persistence; Docker pause/resume or Apple unsupported response |
| ExpirationRenewal | Sandbox survives the superseded deadline, then really stops after the renewed deadline; native state confirms the stop |
| ConcurrentSandboxOperations | Three real sandboxes receive overlapping renew-expiration and exec requests, then each must stay running and return its own successful command result |
| MCPOverHTTP | SDK initialization and tool discovery, sandbox creation, command execution/logs, file write/read, deletion through authenticated streamable HTTP |
| AbruptProcessRecovery | SIGKILL the OpenSBX process while a sandbox and command are live, restart against the same DB, verify native identity, files and command history, then stop and delete it through the recovered API |
| AbruptExpirationRecovery | SIGKILL the process while a short-TTL sandbox is live, wait past its persisted deadline, restart, then verify the listener remains available while same-process background recovery stops the exact owned resource and clears its deadline |
| ShutdownAndPersistence | SIGTERM exits within budget and stops the native sandbox, restart using the same DB preserves identity/history/files, API deletion clears state |

The domain test passes the **exact URL returned by OpenSBX** to curl, without a
Host override, `--resolve`, `--connect-to`, or a custom dialer. It checks the
execution-specific app response at `/`, `/v1/health`, `/swagger/index.html`, and
`/v1/mcp`. These sandbox-host paths must never reach management handlers. Stopped
and deleted sandboxes must no longer serve the app. Curl's built-in localhost
handling is exercised; this is not a browser compatibility or OS DNS resolver test.

## Isolation and cleanup

Each run uses a fresh temporary directory, separate SQLite database/catalog/logs,
API key, native IDs, and OS-assigned listener ports. It does not open the user's
OpenSBX database. The runtime image catalog is snapshotted before materialization.

Cleanup is registered before process startup and runs even after assertion
failures. It reads ownership from the isolated database (including recovery rows),
deletes through the API, stops the process, and verifies native absence. If API
cleanup fails, an exact-ID native fallback is attempted; that failure still fails
the test. Both sandbox and command table counts must be zero; assertions open the
existing database read-only, without migrations or repairs. Newly introduced
private cache references are removed and pre-existing references checked. Finally
the temporary database/catalog directory is removed and its absence verified.

No global prune, runtime service stop, or deletion of unrelated resources is used.
An unresponsive runtime can prevent resource removal: the suite reports that as a
cleanup failure, not success. Abruptly killing the test runner bypasses Go cleanup;
allow the per-operation and suite deadlines to complete normally.

Set `OPENSBX_E2E_ARTIFACTS=/path/to/diagnostics` to retain process logs outside the
temporary state directory. CI uploads these logs even on failure. Without this
option, failed runs print process logs to the test output. Databases and workload
archives are not retained as artifacts.

The existing prepared-store cross-runtime portability test remains complementary:
it checks shared-image identity and offline native-cache recovery across both
backends. See [Contributing](../CONTRIBUTING.md) for its invocation.

Abrupt-failure cases deliberately kill only the OpenSBX server process; they do
not kill the runtime or the test runner. The general recovery case verifies that
resources survive and remain manageable after restart. The expiration-recovery
case waits past the saved absolute deadline while the server is down, then verifies
startup stops the exact test-owned resource. Both cases remove their exact
test-owned resources afterward.

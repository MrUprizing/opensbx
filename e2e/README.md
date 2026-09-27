# Real-process end-to-end suite

This suite compiles `cmd/api`, launches the resulting binary as a separate
process, and drives its public REST and MCP interfaces over a real loopback
listener. Runtime operations use actual Docker containers or Apple containers.
The `e2e` build tag keeps these tests out of `go test ./...`.

CI runs this package with `-race` to check the harness too. The child server is
built normally; application race coverage also runs in the default race suite.

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

## Scenarios

| Subtest | Assertions |
| --- | --- |
| AuthenticationAndOrigins | Health/readiness, valid API key, missing/wrong keys rejected on REST and MCP, foreign Origin rejected |
| ImagesPullExportImport | Missing image rejected, explicit pull/list/inspect, CLI export/remove/import/inspect, selected OCI manifest preserved |
| SandboxCommandsFilesDomainAndLifecycle | Actual running resource and public/native identities, inventory/network/stats, stdout/stderr/exit code, log streaming, kill, file CRUD, app URL, stop/start/restart and file persistence; Docker pause/resume or Apple unsupported response |
| ExpirationRenewal | Sandbox survives the superseded deadline, then really stops after the renewed deadline; native state confirms the stop |
| MCPOverHTTP | SDK initialization and tool discovery, sandbox creation, command execution/logs, file write/read, deletion through authenticated streamable HTTP |
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

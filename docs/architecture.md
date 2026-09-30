# Architecture

```text
REST / MCP models
        |
internal/api: transport and domain conversion
        |
internal/service: application policy
        |
internal/sandbox: runtime, process, filesystem and cache contracts
        |
internal/runtimeio: native IDs, ownership and runtime records
        |
Docker engine client or Apple container CLI
```

```text
Same opensbx binary: CLI shortcuts / resource groups
        |
internal/cli: shared metadata, argument validation, rendering and observation
        |
internal/client: authenticated, redirect-free, proxy-free loopback HTTP
        |
internal/api -> service -> runtimeio -> selected runtime (server process only)

Standalone image CLI -> internal/images (no server/runtime required)
```

## Boundaries

- `models` defines the public REST/MCP JSON contract. Domain and runtime packages
  use their own types; HTTP DTOs stay at the API/client/CLI transport edges.
- `internal/cli` constructs a fresh Cobra command tree per invocation, with native
  command groups, persistent/local pflag bindings, contextual help and completion
  generators/providers. Separate shortcut/group nodes share operation handlers.
  Exec splits argv with ArgsLenAtDash and preserves every guest value after `--`.
  It resolves names/unique ID prefixes
  once, renders human/JSON/quiet output and coordinates cancellable observations.
- `internal/client` performs local HTTP operations without importing service,
  runtime or database packages. It validates loopback IP/port, bypasses proxies,
  refuses redirects and never automatically retries mutations. Ordinary requests
  have a five-minute total/header budget; connection establishment is bounded to
  five seconds. Wait/log streams have a separate ten-second header budget and
  caller-controlled body lifetime. Connections are single-request so Transport
  cannot implicitly retry a mutation on a stale reused connection. Lost mutation
  responses retain a sanitized underlying cause and report uncertain outcome
  rather than implying no work ran.
- The daemon remains the sole owner of runtime connections, SQLite locks, recovery
  and TTL. Management never starts a server or pulls an image implicitly. Root
  start/stop remain daemon operations; sandbox start/stop are HTTP resource operations.
- `internal/clicompat` only retains explicitly named legacy single-dash long flags
  using the Cobra tree's flag arity, never rewrites values/guest argv, and records
  output errors native help printers cannot return. A structural guard rejects a
  required value that would consume an unescaped `--`; actual exec validation and
  splitting still use Cobra ArgsLenAtDash. It is not a second value/flag parser.
  The obsolete `internal/cliflags` adapter/manual command lookup is removed.
- Daemon hooks are injected by `cmd/api`, so the Cobra CLI owns root start/stop
  and foreground flag parsing without importing runtime/service constructors.
  Hooks receive serialized parsed server options; existing config/daemon behavior
  stays in the binary. Offline image operations mount their own Cobra subtree.
- Native completion providers account for pinned Cobra/pflag's double-parse
  separator metadata: execution uses ArgsLenAtDash, while completion guards capture
  only the actual completed literal-separator boundary. No server query occurs
  during help/script generation or after guest arguments begin.
- `internal/terminaltext` escapes untrusted human metadata/diagnostics without
  changing raw guest streams, file reads or JSON values. Directory layout retains
  newline/tab but cannot introduce terminal escape effects.
- `internal/service` owns application policy and sandbox creation compensation.
- `internal/sandbox` defines the interfaces implemented by the runtime adapters.
- `internal/runtimeio` translates public IDs into native references and keeps
  adapter-specific persistence details out of the service.
- `internal/images` manages a shared OCI catalog independent of the selected
  runtime's native image cache.

When adding behavior, start at the layer that owns it: HTTP or MCP conversion in
`internal/api`, shared policy in `internal/service`, runtime-specific behavior in
the relevant adapter, and OCI image operations in `internal/images`.

## Invariants

- The public REST/MCP contract is independent of Docker and Apple identifiers.
- Runtime operations require recorded ownership; listing native resources does
  not adopt them.
- Sandbox creation retains a bounded compensation/recovery path if adoption fails.
- Create, start, restart, stop and delete use durable, runtime-scoped operation
  intents before native mutation. This is recoverable orchestration, not a
  distributed transaction.
- Ownership and command history are deleted together. Command registration shares
  the runtime lifecycle lock with removal; provenance updates cannot recreate a
  deleted owner or overwrite its expiration.
- App-host routing happens before path routing; a sandbox URL cannot reach
  management API or MCP routes.
- The image catalog is separate from runtime caches. Pull/import validate OCI
  content before publication; retagging cannot redirect an already-resolved image.
- Registry requests validate destinations and do not forward credentials across
  hosts. Custom cross-origin token/CDN services fail closed by default.
- OCI images represent filesystem/configuration, not running process snapshots.
  Checkpointing and snapshot portability are not implemented.

## Command identity and bounded observations

Docker command creation probes shell builtins and readable `/proc` with a five-second
bounded helper before starting a payload, without a permanent capability cache.
Confirmed absence retains direct original argv with command-specific unsupported
signals; native errors/timeouts are not absence and abort before payload creation.
Positive capability selects an optional constant-source wrapper, retaining original
persisted metadata/cwd/argv. Its first stderr record has a nonce/guest PID/start-time
identity, filtered under a 256-byte bound; remaining stderr and all stdout pass through.
DB registration and tracking precede payload attach. Invalid/missing protocol never
starts another copy. Demux errors wake readiness and propagate to stream readers/wait
without claiming an unconfirmed native process exited.

Effective `Config.Env` plus request overrides are carried as quoted positional
restore data. Internal startup suppresses ENV/BASH_ENV; the payload receives their
original values. Subshell-only scratch variables preserve caller exports. Shell-owned,
readonly or non-exportable explicitly configured environment values select direct
exec to avoid silently changing them. Signal helpers inherit only the container
environment with startup hooks suppressed; they never cancel the payload's context.

Docker's ExecInspect PID acknowledges startup only: it is not a guest signal target.
The signal path waits for native startup and verified identity, then checks `/proc`
start-time and uses builtin kill with a single positive guest PID. Apple's existing
wrapper/handshake remains unchanged. These checks narrow but do not eliminate the
guest-local kernel check-to-kill/PID-reuse race and never target descendants as a group.
Neither observation failure nor local capture overflow authorizes a sandbox/server
kill. See [Docker helper protocol](docker-command-identity.md) for fixture contracts.

Log retention remains bounded (Docker 1MiB/stream, Apple 256KiB/stream). A streaming
reader that missed output returns `sandbox.ErrLogTruncated` before reading any
overwritten tail. API log readers emit available chunks up to32KiB, retaining only
an incomplete UTF-8 suffix. Truncation/source failures become `{type:"error",data}`
records and close/join both sources. Snapshot endpoints retain bounded-tail semantics;
they do not certify complete history. The client bounds encoded NDJSON records to
1MiB, and foreground JSON capture shares an 8MiB aggregate stdout/stderr budget.
Exceeding either fails observation and cancels attachment, without partial successful
JSON, guest signaling or inferring terminal success from EOF.

## Startup recovery and expiration

The server explicitly calls the selected client's `Recover(ctx)` after acquiring
process/database locks and validating the runtime and image store, before opening
listeners. Constructors do not perform recovery. All durable intents and deadlines
are registered before native work. Synchronous startup warm-up has an independent
one-second budget; exhausting it defers work rather than terminating the server.
Explicit caller cancellation and database/ownership/invalid-intent errors remain
startup failures. Operational native errors are logged and deferred.

Incomplete creation is rolled back only by its exact persisted native reference
and ownership token (Docker also persists its chosen name before creation).
Creation intents survive until provenance adoption commits. Delete retries also
work when the native resource is already missing. Start/restart recovery observes
the actual state and refreshes ports; it does not blindly restart a workload again.
Required database writes and intent completion share a transaction. A failed
operation retains its intent and blocks conflicting lifecycle changes until
recovery. Failed work is retried in the same process every 30 seconds, independently
of its TTL or request cancellation. Each background attempt has a two-second
budget including lifecycle-lock acquisition. The dispatcher runs one attempt at a
time with a 100ms yield between attempts so stalled recovery cannot continuously
hold the lifecycle lock against API traffic. Shutdown cancels recovery workers
before acquiring that lock and leaves unfinished durable work for the next process.

Successful creates awaiting service adoption are not registered for rollback.
Only failed create/adoption/compensation paths (or startup's interrupted intents)
authorize that recovery. Workers re-read the exact intent ID under the lifecycle
lock; a retired or superseded attempt cannot roll back a newer successful mutation.

TTL is an absolute persisted deadline. Recovery restores future deadlines without
extending them and attempts to stop overdue resources. Transient overdue-stop
failures are logged and retried after 30 seconds without changing the original
deadline. Stop/remove clear the deadline only with successful metadata persistence.
Legacy rows without runtime/deadline metadata are not automatically adopted or
expired; explicit lifecycle operations continue to support them. Recovery never
replays another backend's intents or discovers new ownership from an inventory.

## Proxy admission during recovery

`Service.Route` admits a route only while ownership still matches the selected
runtime, no lifecycle intent is pending, the durable deadline is not expired
(or absent on legacy rows), and the native routing snapshot reports running.
Ownership, pending work and expiration are checked before native resolution and
again after its singleflight result; DB lookup failures deny admission. This
also blocks unconfirmed start/restart and unknown intent kinds, not only deletes.
Management inspection and recovery remain usable, as do unrelated healthy routes.

This isolates newly admitted OpenSBX proxy requests, not the runtime itself: a
resource can continue running until cleanup succeeds. It does not revoke already
admitted requests/open connections or restrict direct access to native loopback
ports. No lifecycle lock is held across proxy traffic or native route resolution.

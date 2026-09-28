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

## Boundaries

- `models` defines the public REST/MCP JSON contract. Domain and runtime packages
  use their own types; HTTP DTOs stay at the API edge.
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

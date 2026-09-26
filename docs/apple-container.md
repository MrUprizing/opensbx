# Apple container backend

## Selection and installation requirements

One backend is selected for the entire instance. REST routes, MCP tools, request
and response models, and error mappings are unchanged.

```sh
opensbx -runtime docker
opensbx -runtime container
```

An explicit flag bypasses the menu. Without it, macOS with a terminal on stdin
shows an English Docker / Apple container menu. Enter or an empty EOF selects
Docker; invalid input is retried (invalid input at EOF fails startup).
SIGINT/SIGTERM abort selection. Non-terminal input and other operating systems
default to Docker without reading stdin. There is no runtime environment variable.

Apple requirements:

- Apple Silicon (`arm64`) running macOS 26 or later.
- Apple `container` **CLI and server 1.4.1**, installed independently and already
  running for the same macOS user. This adapter pins the validated JSON contract;
  a different version fails validation instead of guessing its schema.
- `container` available on the launching process's PATH. Its absolute executable
  is resolved once. Opensbx does not install/upgrade it or start global services.
- Locally pulled `linux/arm64` images. Use the existing image-pull API first.
- Guest images must provide `/bin/sh`, Linux `/proc`, `sleep infinity`, and basic
  utilities (`mkdir`, `dirname`, `mv`, `cat`, `rm`, `rmdir`, `ls`, and shell `kill`). There is
  no guest agent, host SDK, or runtime compiler requirement.

For a user LaunchAgent, set an explicit `-runtime container`, configure PATH to
include the CLI installation directory, and use a stable working directory.
Run under the same logged-in user whose container service is registered; this is
not a system/root LaunchDaemon integration. HOME and native user launch context
are retained for CLI access; arbitrary host environment and `CONTAINER_*`
overrides are not forwarded. A failed platform, CLI, health, or version check
aborts **before HTTP listeners open**, without falling back to Docker.

Verify existing capabilities without changing them:

```sh
uname -m
sw_vers -productVersion
command -v container
container system status --format json
```

The existing installer does not provision Apple container. Build a binary from
this source or use a release that includes the backend; older releases do not
gain support from these instructions.

## Ownership and persistence

Docker continues to use `sandbox.db`. Apple uses `sandbox-container.db` in the
working directory. Do not rename/copy one database over the other. Use one
opensbx instance per database; simultaneous instances sharing it are unsupported.

Sandbox IDs are random `opensbx-<32 hex>` names, also recorded in an ownership
label. Operations require both the selected repository record and matching live
ownership label. Listing does not adopt unrelated containers. Shutdown and TTL
stop only recorded, owned sandboxes. No prune, global stop, or `--all` mutation is
used. Image operations address the explicit requested image in the user's image
store, as with Docker; listing images is not restricted to sandbox-owned images.

TTL defaults to 900 seconds and stops rather than deletes a sandbox. Start and
restart establish a fresh default TTL; renewal replaces it. Timers and observed
finish timestamps are in memory, not durable scheduling across server crashes.
Graceful shutdown attempts to stop all recorded owned sandboxes. A native start
timestamp is reported when available; unknown finish times remain empty rather
than being fabricated.

## Resource and networking limitations

- CPUs default to 1 and must be **whole numbers from 1 through 4**. Fractional
  CPUs are rejected, never rounded. Memory defaults to 1024 MiB, maximum 8192 MiB.
- **Pause/resume are unsupported**. They return clear backend errors using the
  existing handler mapping (`500 INTERNAL_ERROR`), not new 501/400 mappings.
- Published guest ports must be 2–65535, TCP or UDP, maximum 128 per sandbox.
  Every host publication explicitly binds `127.0.0.1`, never `0.0.0.0`.
- Host ports are dynamically selected while temporary loopback sockets are held.
  Closing those sockets before the CLI binds is **not an atomic reservation**.
  Creation/start is serialized within an instance and retried at most three
  times, rolling back only the newly generated resource on failure. Failed
  rollback retains a repository recovery record when persistence is available.
- Start/restart of an existing sandbox never deletes/recreates its filesystem to
  repair a port collision. Such failures are returned to the caller. Proxy
  mappings preserve `guest-port/protocol -> localhost host port`.
- Images are checked locally before creation; a missing image returns the same
  image-not-found sentinel as Docker. Apple offers no create `--pull=never`
  switch: external CLI image deletion between the check and create cannot be
  coordinated by opensbx. Do not mutate managed resources concurrently outside
  opensbx. API image deletion and creation are serialized.
- Apple's image `--force` only ignores missing references; it is not Docker's
  force-removal behavior. No dependent container or other image tag is deleted
  to emulate it. A digest with multiple matching references requires an exact
  tag, avoiding ambiguous deletion. Execution targets the native ARM64 variant.

## Commands, signals, files, and resource statistics

Commands use attached non-TTY CLI execution, not `exec --detach`. The latter
returns a container ID, not a process ID, and loses attached output. An HTTP
disconnect does not cancel an accepted command. Wait requests may be canceled
without killing the guest process.

The fixed guest wrapper creates a private randomly named metadata directory,
records its PID and `/proc` starttime, and waits for a host stdin handshake before
`exec` replaces it with the requested executable. User argv stays positional.
Kill looks up that command in the selected sandbox, checks the saved PID's
starttime again, and sends an explicit **Linux** numeric signal (1–64), including
9, through a separate guest exec. It never uses broad `pkill`, native CLI signal
forwarding, or whole-sandbox stop to kill one command. Identical sibling commands
have different tracked identities. Signals target the executable process, not a
guaranteed recursive process-tree termination.

**Residual race:** a process can exit between the `/proc` check and `kill`, and a
kernel could reuse that PID in that interval. This is not a race-free pidfd
implementation. Processes/files in one sandbox are one trust domain; malicious
guest code can interfere with its own metadata or siblings. No user-provided
host mount/path is introduced. Native signal forwarding is deliberately avoided
because 1.4.1 has the upstream signal encoding mismatch described in
[apple/container#1941](https://github.com/apple/container/issues/1941).

Per command, the last 256 KiB of each stdout/stderr stream is retained. Up to 128
commands' live/log state is retained per instance, evicting completed commands
first; if all 128 are active, new execution is rejected. Slow log consumers can
miss overwritten bytes. Command metadata/exit codes persist in SQLite; logs and
live process identities do not survive server restart. Missing retained logs or
an untracked running command produce explicit errors, not invented results.
Native CLI diagnostics share stderr with guest stderr; CLI transport failures and
guest exit codes cannot always be distinguished solely by that channel. A
successful signal operation confirms delivery was requested, not process exit;
use wait to observe completion.

Caller-controlled command input has a combined limit of **64 KiB and 1024
entries**, checked before any command CLI call or history persistence. The
executable, each argument, a nonempty working directory, and each `KEY=VALUE`
environment pair count as one entry each. The byte budget counts decoded UTF-8
bytes plus one NUL terminator per entry; an environment pair includes the `=`.
Fixed internal CLI flags/scripts and JSON wire encoding are not part of this
budget. Sandbox creation environment entries have their own **64 KiB / 1024
entry** combined limit, using the same byte accounting. Exceeding either limit
returns a backend error through the existing API/MCP error mapping.

The 128-command limit bounds **live/log memory, not persistent history**. SQLite
keeps command metadata, arguments, and exit results until the sandbox is deleted;
there is no automatic history expiry, row-count quota, or silent eviction of
persisted records. A long-lived sandbox with many commands can grow the database
and history responses indefinitely, even with the per-command input bound. JSON
escaping and database overhead may also make stored data larger than that bound.
Monitor database/disk usage and delete unneeded sandboxes through the API. Apply
an external filesystem/disk quota or an operational database-size limit if
required; these are not provided by opensbx. Deleting rows does not necessarily
shrink the SQLite file immediately.

Text-file operations pass paths positionally to fixed scripts inside the guest
and contents via stdin. Option-shaped paths are made explicitly relative. No
user input is interpreted by a host shell. Guest nonzero exits are failures.
Writing creates missing parent directories. Deletion recursively removes the
exact requested guest path and succeeds if it is already absent, matching the
Docker backend.
File contents and per-call JSON/text output are bounded at 4 MiB. Overflow is
reported rather than returning truncated JSON or file content; output continues
to be drained without unbounded capture. Native failure diagnostics are bounded
and not reflected in synchronous API errors. CLI calls have a two-minute upper
bound (a shorter caller deadline still applies).

Stats take **two** independent `stats --no-stream --format json` observations
and calculate CPU delta divided by elapsed monotonic host time (100% = one
fully used core). Each CLI observation internally waits about two seconds, so
an API sample normally takes at least four seconds. Empty/partial samples,
identity mismatches, reversed counters, and invalid time intervals fail rather
than manufacturing a zero percentage. Timing includes CLI overhead; Apple's
internal sample fallback is not exposed in its JSON, so these are approximate
measurements, not kernel-timestamped samples.
Sampling holds no global lifecycle mutex, so unrelated Stop/TTL operations can
proceed while stats wait for the CLI. Ownership and running state are rechecked
after sampling, and a changed native start timestamp invalidates the result.
Sandbox listing reconciles repository rows against one CLI inventory snapshot;
missing resources remain listed as removed without deleting their history.

## Testing and validation status

Hermetic tests cover the Apple backend, injected REST/MCP contracts, startup
preflight, ownership, bounded output, command lifecycle, file scripts, and
stats/list regressions. Run them without installing or starting a runtime:

```sh
go test ./... -count=1
go test -race ./...
go vet ./...
```

`applecontainer.New` accepts a `Runner` (`Run`/`Start`), `Process` (`Wait`/`Kill`),
and clock. `runtimechoice.Select` accepts OS, terminal state, context, input and
output. These permit hermetic JSON, argv, output-bound, time-delta, and lifecycle
tests without a local daemon. They do **not** prove real CLI/daemon behavior.

**Real Apple runtime validation has not been performed:** the validation host
has no Apple CLI available, and its Docker daemon is also unavailable. The
automated Apple smoke test is explicitly opt-in and is not part of the commands
above. On Apple Silicon macOS 26 or later, with CLI/server **1.4.1** already
installed and running and `node:25-alpine` already available locally as a
`linux/arm64` image, run:

```sh
OPENSBX_APPLE_INTEGRATION=1 OPENSBX_APPLE_TEST_IMAGE=node:25-alpine go test -tags appleintegration ./internal/applecontainer -run '^TestAppleRuntimeEndToEnd$' -count=1 -v
```

The test does not install software, pull images, or start services. It uses a
temporary database, generates its own sandbox IDs, and cleans up only those
exact IDs. It checks prerequisites before creating resources; use a dedicated
test environment and do not concurrently modify its image or generated
sandboxes through external tools. This live test has **not** been run on the
current validation host. Do not treat the backend as production-proven until
it passes on the supported runtime.

Required live checks use only generated disposable IDs: identical sibling
commands with distinct outputs, SIGTERM and SIGKILL affecting only the selected
command, early exit/handshake failure, file argv injection attempts, localhost
port allocation/collision rollback, filesystem-preserving restart, TTL, and
cleanup of precisely those IDs. Never run a global prune or service stop.

Contract references: [tag 1.4.1](https://github.com/apple/container/tree/1.4.1),
[inspection](https://github.com/apple/container/blob/1.4.1/docs/container-inspection.md),
[CLI commands](https://github.com/apple/container/blob/1.4.1/docs/command-reference.md),
[resource statistics](https://github.com/apple/container/blob/1.4.1/Sources/ContainerCommands/Container/ContainerStats.swift).

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
- Managed `linux/arm64` OCI images. Use the image-pull API or runtime-independent
  `opensbx image import` first. Native cache images are not automatically adopted.
- The configured **vminit image must already be available locally**, in addition
  to the workload. For the validated default configuration this is
  `ghcr.io/apple/containerization/vminit:0.45.0`. Prepare this trusted runtime
  dependency explicitly; OpenSBX never downloads it implicitly during Create.
- A compatible default kernel must already be configured and available locally.
  Kernel installation/configuration is an explicit Apple runtime setup operation,
  not an automatic OpenSBX download. Use the runtime's `container system kernel set`
  workflow with a trusted kernel/archive; consult `container system kernel set --help`
  for the pinned version's options before preparing the host for offline use.
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

New execution state uses separate Docker/Apple databases under the common data
directory. Existing working-directory databases require explicit compatibility
mode and a backup; see [migration](deployment.md). Public IDs and command history
are retained. An exclusive DB lock prevents simultaneous service instances.

Sandbox IDs are random `opensbx-<32 hex>` names, also recorded in an ownership
label. Operations require both the selected repository record and matching live
ownership label. Listing does not adopt unrelated containers. Shutdown and TTL
stop only recorded, owned sandboxes. No prune, global stop, or `--all` mutation is
used. Image operations use the independent OpenSBX OCI catalog, not the user's
native image inventory. Native cache import touches only private content-derived
OpenSBX references; image unreference never deletes native cache images.

TTL defaults to 900 seconds and stops rather than deletes a sandbox. Start and
restart establish a fresh default TTL; renewal replaces it. Timers and observed
finish timestamps are in memory, not durable scheduling across server crashes.
Graceful shutdown attempts to stop all recorded owned sandboxes. A native start
timestamp is reported when available; unknown finish times remain empty rather
than being fabricated.

## Resource and networking limitations

- CPUs default to 1 and must be **whole numbers from 1 through 4**. Fractional
  CPUs are rejected, never rounded. Memory defaults to 1024 MiB, maximum 8192 MiB.
- **Pause/resume and fractional CPUs are unsupported**. The application checks
  neutral runtime capabilities and returns `400 BAD_REQUEST`, without rounding.
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
- Creation resolves a managed platform manifest before loading its complete local
  OCI archive. A local `image save` roundtrip verifies manifest/config identity;
  an import-generated native index is not treated as the source root digest.
- Apple 1.4.1 has no `--pull=never`, so creation passes
  `--max-concurrent-downloads 0`. In this pinned version, `ClientImage.fetch`
  returns verified local content first, and `ClientImage.pull` rejects zero before
  any registry request: “maximum number of concurrent downloads must be greater
  than 0”. `Flags.ImageFetch` has no earlier validation. A cache miss therefore
  fails closed, not with a silent download. This version-specific behavior needs
  live regression verification whenever the supported Apple version changes.
- The archive is loaded under the exact content-derived named-digest reference,
  not a tag later augmented with a digest (which 1.4.1 does not resolve locally).
  Do not externally mutate owned cache references during creation.

Source contracts: [ClientImage.swift 1.4.1](https://github.com/apple/container/blob/1.4.1/Sources/Services/ContainerAPIService/Client/ClientImage.swift),
[Flags.swift 1.4.1](https://github.com/apple/container/blob/1.4.1/Sources/Services/ContainerAPIService/Client/Flags.swift).

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

Hermetic tests cover typed domain/adapter boundaries, ownership and recovery,
bounded output, command lifecycle, file scripts and stats/list regressions:

```sh
go test ./... -count=1
go test -race ./...
go vet ./...
```

`applecontainer.New` accepts a `Runner` (`Run`/`Start`), `Process` (`Wait`/`Kill`),
and clock. `runtimechoice.Select` accepts OS, terminal state, context, input and
output. These permit hermetic JSON, argv, output-bound, time-delta, and lifecycle
tests without a local daemon. They do **not** prove real CLI/daemon behavior.

**The Apple 1.4.1 live smoke passed in 36.24 seconds.** The previously missing
configured vminit was prepared explicitly before the run:

```sh
container image pull --platform linux/arm64 --progress plain \
  ghcr.io/apple/containerization/vminit:0.45.0
```

This runtime infrastructure image is separate from the OpenSBX workload catalog.
For fully offline creation, both workload data and runtime init/kernel assets must
already be local. A trusted OCI archive can also supply vminit under the configured
reference. Missing prerequisites fail safely; do not remove
`--max-concurrent-downloads 0` to make Create download them implicitly.

The opt-in smoke can reuse an isolated store with a prepared `linux/arm64` image:

```sh
OPENSBX_APPLE_INTEGRATION=1 \
OPENSBX_APPLE_TEST_IMAGE=node:25-alpine \
OPENSBX_APPLE_TEST_DATA_DIR=/absolute/path/to/prepared-isolated-data \
go test -tags=appleintegration ./internal/applecontainer \
  -run '^TestAppleRuntimeEndToEnd$' -count=1 -v
```

Without a supplied data directory, the test uses a temporary store. If the named
artifact is missing, it explicitly pulls that workload into the manager; it never
installs software, starts services, or silently downloads runtime prerequisites.
It uses a fresh execution database and cleans up only generated sandbox IDs and
the exact workload cache reference. Do not concurrently modify those resources.

The successful run verified managed OCI materialization, exact sibling SIGTERM
and SIGKILL isolation, files, TTL/lifecycle, same-port hostname/control routing,
and cache-loss reimport without additional manager registry requests. The trusted
vminit remained installed in the native image store (service count 1, visible
workload image list empty), outside the OpenSBX workload catalog. These results do
not certify Docker live behavior or full browser compatibility. See
[testing](testing.md) for current commands and the bounds of the last full suite.

Contract references: [tag 1.4.1](https://github.com/apple/container/tree/1.4.1),
[inspection](https://github.com/apple/container/blob/1.4.1/docs/container-inspection.md),
[CLI commands](https://github.com/apple/container/blob/1.4.1/docs/command-reference.md),
[resource statistics](https://github.com/apple/container/blob/1.4.1/Sources/ContainerCommands/Container/ContainerStats.swift).

# Use OpenSBX

## Install and start

Install the latest release on Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
```

On Windows, download the matching binary from [GitHub Releases](https://github.com/MrUprizing/opensbx/releases).

Start with the default configuration. On Linux, Docker is selected by default;
on supported Apple Silicon Macs, OpenSBX lets you select Docker or Apple
Container:

```sh
opensbx start
```

The server starts in the background and binds to `127.0.0.1:18089` by default.
Use `opensbx start -runtime container` to select Apple Container, or `-addr` to
change the address. Stop the server with `opensbx stop`. See [runtime setup](runtimes.md).
The server binds to loopback only. API, MCP, Swagger and sandbox app URLs share this listener.

Use `opensbx -h` or `opensbx -help` for the command list and options. Add `-h`
to a command, such as `opensbx start -h`, for command-specific help.

## Create and use a sandbox

Prepare an image explicitly. Images already cached by Docker or Apple are not
automatically imported into OpenSBX:

```sh
opensbx image pull node:22
```

Create a sandbox with the image, an exposed app port and a 15-minute timeout:

```sh
SANDBOX_ID=$(opensbx create node:22 --port 3000 --ttl 15m --quiet)
opensbx ls
opensbx inspect "$SANDBOX_ID"
opensbx exec "$SANDBOX_ID" -- node --version
```

When a sandbox exposes a TCP port, its response includes an app URL such as
`http://<sandbox>.localhost:18089`. Delete the sandbox when finished:

```sh
opensbx rm "$SANDBOX_ID"
```

Sandboxes default to 1 CPU, 1 GiB memory and a 15-minute timeout. Limits are 4
CPUs and 8 GiB memory.

## Command-line reference

The six shortcuts share handlers with their grouped forms:

| Shortcut | Grouped form |
| --- | --- |
| `create IMAGE` | `sandbox create IMAGE` |
| `ls` | `sandbox list` |
| `inspect TARGET` | `sandbox inspect TARGET` |
| `exec TARGET -- PROGRAM ARGS...` | `command exec TARGET -- PROGRAM ARGS...` |
| `logs TARGET COMMAND_ID` | `command logs TARGET COMMAND_ID` |
| `rm TARGET` | `sandbox remove TARGET` |

`TARGET` is an exact generated name, full ID or unique ID prefix, resolved once
per operation to a stable ID. Ambiguity fails with matching choices; the CLI never
selects the first or most recent resource. Command IDs must be complete. There is
no custom-name persistence option.

Use `opensbx --help`, `opensbx sandbox --help`, `opensbx exec --help` or
`opensbx help command wait`. Help and completion script generation work offline.
The command tree, flag parser, help and completion are provided by Cobra. Commands
are constructed fresh per invocation; shortcuts and group leaves share operation
handlers, not separate implementations. Use `--long-option` or its documented
single-letter shorthand (`-p`, `-e`, `-q`, `-d`, `-f`, `-h`). Repeatable ports/env
use string arrays: commas in a value such as `--env VALUES=a,b` remain literal.
Flags may precede or follow resource positionals until a literal `--`. Global
management flags also work before the command (`opensbx --addr 127.0.0.1:18090 ls`).
Exec requires `--`: everything after it belongs to the guest, including `--help`,
leading dashes and empty arguments. Shell quoting remains your responsibility.
Value-taking flags must have a value before the separator; a missing value is
rejected before any HTTP request. Use `--option=--` for a literal separator value
(for example, `--cwd=--`), or an explicitly quoted empty value when appropriate.

### Sandbox lifecycle

```sh
opensbx create node:22 --port 3000 -p 8080/tcp --ttl 15m --memory 1GiB --cpus 2 -e NODE_ENV=development
opensbx sandbox stop "$SANDBOX_ID"
opensbx sandbox start "$SANDBOX_ID"
opensbx sandbox restart "$SANDBOX_ID"
opensbx sandbox pause "$SANDBOX_ID"
opensbx sandbox resume "$SANDBOX_ID"
opensbx sandbox renew "$SANDBOX_ID" --ttl 30m
opensbx sandbox stats "$SANDBOX_ID"
opensbx sandbox network "$SANDBOX_ID"
```

Create supports repeatable `--port/-p` (guest ports 1–65535, optional `/tcp` or
`/udp`) and `--env/-e KEY=VALUE`. The backend assigns host ports and enforces its
own limits and runtime capabilities; unsupported Apple operations fail honestly.
`--ttl` uses Go durations such as `30s`, `15m` or `2h`, must resolve to whole seconds
and fit the duration/integer range. Create `0s` uses the backend default; renew
requires a positive TTL. Memory requires explicit units: `MiB/GiB/KiB` are binary,
`MB/GB/KB` decimal, `B` bytes. The result must be an exact whole MiB within 0–8192
(for example, `1GiB` = 1024 MiB; `512MB` is not an exact whole MiB and is rejected).
Backend memory fields are MiB despite historical “MB” labels. `--cpus` accepts
finite fractional values from 0 to 4. Zero resource values use backend defaults;
omitted values retain the existing defaults.

**Daemon versus resource:** root `opensbx start` and `opensbx stop` only control
the server, never a sandbox based on argument count. `sandbox start/stop TARGET`
control a sandbox. No arguments still runs the foreground server; standalone
server flags such as `opensbx -runtime docker -addr 127.0.0.1:18089` remain supported.
Legacy Go-style `-runtime`, `-addr`, `-data-dir`, `-log-file`, `-legacy-db` and
`-help` are narrowly normalized to Cobra long flags where defined. Existing image
single-dash long options are also retained. This is not global token rewriting:
option values and every guest argument after literal `--` remain untouched.

### Commands, output and interruption

```sh
opensbx exec "$SANDBOX_ID" --cwd /tmp -e MODE=demo -- node -e 'console.log(process.env.MODE)'
COMMAND_ID=$(opensbx exec "$SANDBOX_ID" --detach --quiet -- node -e 'setTimeout(() => {}, 30000)')
opensbx command list "$SANDBOX_ID"
opensbx command inspect "$SANDBOX_ID" "$COMMAND_ID"
opensbx logs "$SANDBOX_ID" "$COMMAND_ID"  # add --follow to observe until exit
# Optional: requires backend support for a verified guest process identity
opensbx command kill "$SANDBOX_ID" "$COMMAND_ID" --signal TERM
opensbx command wait "$SANDBOX_ID" "$COMMAND_ID"
```

Foreground exec streams guest stdout/stderr to the corresponding local streams,
waits for a confirmed exit code and exits with the guest's status, including failure.
`--detach/-d` returns details immediately. `--cwd` selects the guest working directory;
repeat `--env/-e`. `command wait` also returns the confirmed guest status. Kill
defaults to TERM and accepts familiar names (with or without `SIG`) or POSIX numbers
1–64. Names use Linux guest signal numbering rather than host numbering.

**Safe signaling:** `command kill` and the foreground Ctrl+C SIGINT request target
only the selected guest process, not matching argv, a process group or all its
descendants. Docker probes `/bin/sh` builtins and readable `/proc` before each
payload (five-second probe budget, no permanent capability cache). On supported
images such as normal node/Alpine images, an internal wrapper records its guest
PID/start-time before `exec` replaces it with the requested program. A bounded
nonce-validated initial stderr preamble is stripped; all subsequent guest stderr
and all stdout are preserved. The signal helper verifies the guest start-time
again and uses builtin `kill` for that single positive PID. Docker's ExecInspect
host PID is a startup acknowledgment only, never a guest signal target.

Confirmed missing prerequisites use the original direct argv instead: shell-free
images still run, but signaling that command reports an unsupported capability.
Timeouts, native failures and invalid protocol responses **do not** silently fall
back. Once a wrapped payload starts, protocol failure never re-executes it. Identity
readiness and each signal helper have five-second budgets (earlier caller deadlines
win). Failed signaling remains fail-closed; the command can still be running.

The wrapper preserves requested argv (including empty/quoted/newline/dash arguments),
cwd, original stored command metadata and exportable environment values. Scratch
calculations are in subshells so exported `stat`, `rest` and `start` are not overwritten.
`ENV`/`BASH_ENV` hooks are disabled for internal shell startup and restored as data
before the payload: hooks execute only when the requested program would invoke them.
To avoid altering explicitly configured shell-owned/readonly values, environments
containing `PWD`, `SHLVL`, `_`, `SHELLOPTS`, shell-specific `BASH*` (except `BASH_ENV`),
`ZSH*`/`KSH*`, or other special shell state such as `UID`, `PPID`, `RANDOM` and
`PIPESTATUS` execute directly without signal support. Non-exportable environment keys
also use direct execution. Shell-created variables not explicitly configured are
not a byte-for-byte environment snapshot guarantee. No external guest utilities
or host-native helpers are installed or required.

Apple retains its existing guest identity handshake. Both runtimes' `/proc`
start-time checks narrow but do not eliminate the guest-local check-to-kill/PID-reuse
race; they are not a host isolation guarantee. After refusal/failure, inspect/wait
explicitly or choose a sandbox lifecycle action deliberately, never as an automatic
fallback. Canceling observation or the helper never cancels the payload; canceling a
helper response cannot undo a signal that was already delivered.

New management commands default to readable output. `--json` returns structured
API-shaped output; foreground exec returns **one** final `{command,stdout,stderr}`
object even for nonzero guest exits, without raw log chunks. `--quiet/-q` prints
only IDs for create, sandbox list and detached exec. `--json` and `--quiet` are
incompatible; foreground exec cannot be quiet. Diagnostics go to stderr.

Ctrl+C on an owned foreground exec cancels observation and makes a bounded,
best-effort SIGINT request only for that command, then exits 130. The CLI reports
the command ID and does not claim termination is confirmed. Inspect or wait again
if needed. Ctrl+C on `command wait` or `logs --follow/-f` only detaches observation;
it does not signal another process, stop the sandbox or stop the server. Transport
failures detach without automatically retrying execution. A wait stream ending
without an exit code is an error, never success. Long observation streams have no
whole-request timeout. Ordinary synchronous HTTP operations have a **five-minute
total budget**, including cold native create/start/restart; an earlier caller
deadline still wins. Connection establishment is limited to **five seconds**,
stream response headers to **ten seconds**, and SIGINT cleanup to **three seconds**.
A lost mutation response reports an **uncertain outcome**, preserving the network
or timeout cause: inspect resources/command history before retrying. It does not
mean the daemon is unavailable, and the CLI never retries the mutation. After
confirmed completion, a log stream that fails to close within
five seconds produces an honest observation error.

Logs default to a snapshot; `--follow/-f` consumes retained NDJSON records.
`--json --follow` is rejected (use a JSON snapshot or raw follow). Logs exist only
while the current server retains them; history surviving a restart does not imply
old logs remain available. Retention is bounded **per command, per stream**:
Docker retains a **1 MiB tail each of stdout and stderr**, and Apple a **256 KiB
tail each**. Docker normally evicts completed in-memory commands after five minutes;
Apple retains up to 128 tracked commands and can evict completed ones to make room.
Snapshot endpoints return only these bounded tails, not a promise of complete
history; an overwritten tail can begin inside a multibyte character.

Streaming emits available text promptly, including carriage-return progress and
final unterminated text, in chunks of at most **32 KiB**, carrying only incomplete
UTF-8 suffixes. Every successful text frame is valid UTF-8. Initial attachment after
history loss or an active reader falling behind produces an explicit **truncation
error**, not a silent jump to a tail. Source failures also produce error records.
The CLI detaches with a failed observation instead of claiming exit 0 with full
logs or emitting successful/partial foreground JSON. Readers are closed and joined
on cancellation, source failure or downstream write failure.

Foreground `--json` captures at most **8 MiB TOTAL** of stdout plus stderr (UTF-8
bytes, not characters). Exactly 8 MiB is accepted. Exceeding it cancels observers
and fails with the command ID and inspect instructions, without signaling the
guest, emitting partial JSON or pretending Ctrl+C occurred. Use raw streaming for
larger output, and attach early enough to avoid overrunning the runtime retention.
The client independently bounds each **encoded NDJSON record to 1 MiB** for both
logs and wait streams. Oversized/malformed records are observation errors. This is
text output, not a binary terminal/PTY or interactive stdin transport.

Human metadata and diagnostics escape terminal controls (including ESC/OSC and
invisible formatting), so hostile names/argv/URLs cannot execute terminal effects.
JSON preserves the original values with JSON escaping. **Raw exec/log stdout/stderr
and file reads remain exact guest data** and can contain terminal controls; redirect
them to a file or use JSON when the producer is untrusted.

### Text files

```sh
printf 'hello\n' | opensbx file write "$SANDBOX_ID" /tmp/message.txt
opensbx file write "$SANDBOX_ID" /tmp/script.js --input ./script.js
opensbx file read "$SANDBOX_ID" /tmp/message.txt > ./message.txt
opensbx file list "$SANDBOX_ID"          # defaults to /
opensbx file list "$SANDBOX_ID" /tmp
opensbx file remove "$SANDBOX_ID" /tmp/message.txt
printf '' | opensbx file write "$SANDBOX_ID" /tmp/empty.txt
```

Write consumes exact UTF-8 text, including empty text, from redirected stdin or
explicit `--input PATH`. Interactive terminal stdin is rejected with an actionable
example rather than waiting indefinitely. Read returns exact unadorned text without
adding a newline. Directory listings preserve newline/tab layout but visibly escape
ESC/OSC and other harmful controls; `--json` retains the wrapper and original text
for both read and list. This is not arbitrary binary transfer or recursive copying.
Remove uses backend deletion semantics (directories may be removed recursively);
review the guest path first.

### Completion

Generate scripts explicitly and load them yourself; no shell configuration is edited:

```sh
# Bash, with bash-completion helpers loaded by your shell
source <(opensbx completion bash)

# Zsh, after completion initialization
autoload -Uz compinit && compinit
source <(opensbx completion zsh)

# Fish
opensbx completion fish | source
```

These commands emit Cobra's native shell generators (not custom scripts) and never
install shell configuration. Bash's generator uses the standard bash-completion
helpers; load the bash-completion package supplied by your system first if needed.
Zsh setup still requires `compinit`; Fish can load the emitted script directly.

Completion derives commands/options from the Cobra tree and uses native dynamic
providers. Resource hints are
read-only, sanitized and best-effort with a 300ms budget against the configured
loopback endpoint; no server is started when offline. Guest paths and command-ID
prefix completion are not inferred. Suggestions respect the typed prefix; no
resource query occurs after guest `--`. Native `__complete` responses include
shell directive records and are an internal protocol, not stable CLI data output.

## Images

The `opensbx image` commands manage OpenSBX's shared local OCI catalog and need
neither a running server nor a runtime. Existing list/inspect JSON remains
unchanged; pull/import/export/remove retain their standalone syntax:

```sh
opensbx image pull node:22
opensbx image list
opensbx image inspect node:22
opensbx image export --output node.tar node:22
opensbx image import --reference node:22 node.tar
opensbx image remove node:22
opensbx image remove node:22 --force
```

For an offline machine, import an OCI layout archive. Docker legacy `save` archives
are not OCI archives. Set `OPENSBX_DATA_DIR` or use `--data-dir` to share a
non-default image catalog with the server. See `opensbx image help` for options.
Commands accept `--data-dir` and `--platform linux/arch[/variant]` (default
`linux/<host architecture>`). Import accepts `--reference`; export requires
`--output` and optionally `--all-platforms` (fails if any required blob is missing).
Flags may precede or follow positionals; `--` ends flag parsing. Remove `--force`
only ignores a missing catalog reference, never deletes native images or pinned
blobs. Pull is always explicit; create never downloads or starts a runtime.

## Configuration

| Setting | Default | Purpose |
| --- | --- | --- |
| `ADDR` / `-addr` | `127.0.0.1:18089` | Loopback address for API and sandbox URLs |
| `OPENSBX_DATA_DIR` / `-data-dir` | `~/.local/share/opensbx` | Image catalog and runtime state |
| `LOG_FILE` / `-log-file` | `opensbx.log` | Server log file |
| `API_KEY` | unset | Optional Bearer token for API requests |
| `-runtime` | Docker | `docker` or `container` (Apple) |

Use `127.0.0.1:<port>` or `[::1]:<port>`; wildcard addresses are rejected. If
`API_KEY` is set, send `Authorization: Bearer <key>`. Keep the data directory
private to the local user, on a local filesystem, and back it up with the server
stopped.

Management uses `--addr`/`ADDR` (default `127.0.0.1:18089`), with a nonzero port and
a numeric loopback IP, including `[::1]:18089`. The client sends `API_KEY` as a
Bearer token, bypasses environment HTTP proxies and refuses redirects so the token
cannot escape to another endpoint. Endpoint errors identify the selected address;
custom ports must match the server. There is no remote/cloud mode, automatic
retry of mutations, automatic image pull or server startup. Only the server owns
runtime connections, SQLite locks, recovery and TTL management.

## API and MCP

- Interactive REST API: `http://localhost:18089/swagger/index.html`
- MCP endpoint: `http://localhost:18089/v1/mcp`
- Health check: `http://localhost:18089/v1/health`

The MCP server also exposes `opensbx://docs/quickstart` and
`opensbx://docs/how-it-works` resources to connected clients.

## Existing installations

If you have an older working-directory database, stop OpenSBX and make a
consistent backup (including SQLite `-wal`/`-shm` files if present). The Docker
database must be named `sandbox.db`; the Apple database must be named
`sandbox-container.db`. Start the same runtime with its matching database path:

```sh
opensbx start -runtime docker -legacy-db /absolute/path/sandbox.db
opensbx start -runtime container -legacy-db /absolute/path/sandbox-container.db
```

Do not rename a database to switch runtimes. Legacy mode updates the database in place;
keep the backup until you have verified the migration. Do not run multiple
OpenSBX processes against the same database.

Existing databases are not copied or adopted automatically. New installations
store runtime databases and the image catalog under `OPENSBX_DATA_DIR`.

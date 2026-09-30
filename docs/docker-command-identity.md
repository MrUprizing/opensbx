# Docker command identity protocol

This is the native helper contract for transport tests and maintainers, not a new
public endpoint. REST/MCP/CLI requests and persisted original command metadata are
unchanged. Implementation constants live in `internal/docker/command_identity.go`.
Tests must model probe/payload/signal separately; classifying every non-`sleep` exec
as a signal is no longer valid. Successful signal and duplicate-command isolation
assertions must remain, not be replaced with universal unsupported expectations.

## Native exec shapes

All three use Docker ExecCreate + ExecAttach with stdout/stderr multiplexing,
no TTY/stdin and no host process lookup. `SOURCE` is the corresponding constant,
never interpolated user shell source. Probe/payload preserve requested WorkingDir.

| Kind | Cmd |
| --- | --- |
| Probe | `[/bin/sh, -c, identityProbeScript, opensbx-probe, NONCE]` |
| Wrapped payload | `[/bin/sh, -c, identityWrapperScript, opensbx-command, NONCE, KEY=VALUE..., --, PROGRAM, ARGS...]` |
| Direct payload | Original `[PROGRAM, ARGS...]` |
| Signal | `[/bin/sh, -c, identitySignalScript, opensbx-signal, GUEST_PID, STARTTIME, SIGNAL_NUMBER]` |

`NONCE` is the generated `cmd_` + 40 hexadecimal command ID, unique per accepted
command/probe. Restore pairs are sorted by key from effective original Config.Env
merged with request overrides. Probe/wrapped exec Env contains sorted request
overrides excluding ENV/BASH_ENV, then `ENV=` and `BASH_ENV=`. Signal Env is only
those two hook-suppression overrides; normal Docker inheritance still applies.
Direct payload Env is the sorted original request overrides, unmodified.

## Probe

- Five-second total helper context covers create/attach/demux/inspect. Output is
  bounded to **256 bytes per stream**; native/transport/output/protocol failures
  return errors, not capability-negative retries.
- A successful probe emits exactly this stdout line and no stderr, then exits 0:
  `OPENSBX_PROBE NONCE GUEST_PID STARTTIME\n`.
- `GUEST_PID` is canonical decimal, greater than 1 and within signed 32-bit range;
  `STARTTIME` is canonical positive uint64 decimal from Linux stat field 22.
- Exit 125 is the constant script's confirmed missing shell builtin/proc prerequisite.
  A native error explicitly reporting missing `/bin/sh` also permits direct payload.
  Arbitrary daemon errors, permission errors, cancellation, timeout, malformed
  success output and other exit codes do not permit fallback.
- Explicit shell-owned/readonly or non-exportable environment values select direct
  execution before probing, preserving compatibility rather than altering values.

## Payload and filtering

- DB/tracking exists before payload attach. The native inspect PID remains a host
  startup acknowledgment, independent of the guest PID in the protocol.
- Wrapped payload emits **one initial stderr line**:
  `OPENSBX_EXEC NONCE GUEST_PID STARTTIME\n`, then restores environment and `exec --`
  replaces the same process with the original argv. Calculations occur in a subshell;
  `$$` intentionally identifies the parent wrapper which will exec the payload.
- The stderr writer buffers at most **256 bytes including the newline**. Validate
  tag, exact nonce and canonical numeric fields. Tolerate fragmented/coalesced Writes.
  Strip only this initial line; forward the coalesced remainder and every later byte
  unchanged, even if payload text repeats the protocol tag/nonce. stdout is separate.
- Publish PID/start/error under the tracking mutex and close identityReady once.
  Missing, malformed, oversized or nonce-mismatched preambles wake waiters with an
  error. Demux errors are stored and propagated after retained data via error-closing
  ring readers. Failed protocol never re-executes a fallback payload.

## Signaling

- Keep wrong-sandbox/finished/pre-cancel/invalid-signal/native-start guards.
  Identity readiness has a separate five-second bound, reduced by the caller deadline.
- Helper arguments are server-validated numeric data. Builtins read `/proc/PID/stat`,
  compare start-time, and `kill -SIGNAL PID` sends to one positive guest PID only.
  No argv patterns, groups, host namespace PIDs or payload context cancellation.
- After builtin kill succeeds, helper stdout is exactly
  `OPENSBX_SIGNAL GUEST_PID STARTTIME SIGNAL_NUMBER\n`, with no stderr. Exit 0 alone
  without this exact acknowledgment is not signal success (and cannot establish
  whether a lost response occurred before or after the signal).
- Exit 124 means missing/mismatched target identity; 125 means invalid helper
  input/prerequisite. Any nonzero/helper transport failure is not confirmed signal
  success. After successful helper exit, wait up to 500ms for payload completion and
  return current original command detail (nil exit code is valid for nonterminal signals).
- A canceled helper connection cannot retract an already sent signal and never
  cancels the payload. `/proc` validation has the documented guest-local check-to-kill
  race, not pidfd-style atomic targeting.

## Required independent coverage

Keep original successful signaling assertions with helper-aware fixtures. Cover
positive probe, confirmed absence/direct execution, native timeout/error without
payload, env/cwd/empty/dash/newline argv preservation, suppressed/restored hooks,
split/coalesced/invalid preamble, repeated later protocol text, demux failure,
identity readiness/finished/cancellation, single-PID helper failure and two identical
commands receiving an isolated signal. Unit fixtures do not prove live Docker behavior;
the isolated real-process E2E must verify normal node/Alpine signaling subsequently.

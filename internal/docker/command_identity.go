package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"opensbx/internal/sandbox"

	"github.com/moby/moby/api/pkg/stdcopy"
	moby "github.com/moby/moby/client"
)

const identityPreambleLimit = 256
const identityHelperTimeout = 5 * time.Second

// All scratch variables and stat field splitting live in a subshell. This must
// not overwrite caller-exported stat/rest/start (or the payload's positional argv).
// Linux stat's comm can contain parentheses: remove the last ") " delimiter.
const identityBuiltinsScript = `
set -f
IFS=' '
for builtin in read printf kill set shift export unset exec '['; do
  case "$(command -v "$builtin")" in "$builtin") ;; *) exit 125;; esac
done
`

const selfIdentityScript = identityBuiltinsScript + `
(exec --) || exit 125
IFS= read -r stat < "/proc/$$/stat" || exit 125
rest=${stat##*) }
start=$(set -- $rest; [ "$#" -ge 20 ] || exit 125; shift 19; printf '%s' "$1") || exit 125
case "$start" in ''|0|*[!0-9]*) exit 125;; esac
[ "$$" -gt 1 ] || exit 125
`

// Exit 125 is a confirmed missing prerequisite, not a native/transport failure.
const identityProbeScript = `(` + selfIdentityScript + `
printf 'OPENSBX_PROBE %s %s %s\n' "$1" "$$" "$start"
)`

// Cmd positional layout: nonce, original effective KEY=VALUE pairs, --, payload
// argv. Hook variables are suppressed only during internal shell startup and
// restored before exec; requesting an actual shell still gets its own hooks.
const identityWrapperScript = `(` + selfIdentityScript + `
printf 'OPENSBX_EXEC %s %s %s\n' "$1" "$$" "$start" >&2
) || exit 125
shift
unset ENV BASH_ENV PWD OLDPWD SHLVL _
while [ "$1" != '--' ]; do
  export "$1" || exit 125
  shift
done
shift
exec -- "$@"
`

// A positive, single guest PID only. This is guest-local PID/starttime validation,
// not a pidfd: the check-to-kill kernel race is the same limitation as Apple.
const identitySignalScript = `(` + identityBuiltinsScript + `
case "$1:$2:$3" in *[!0-9:]*) exit 125;; esac
[ "$1" -gt 1 ] || exit 125
IFS= read -r stat < "/proc/$1/stat" || exit 124
rest=${stat##*) }
start=$(set -- $rest; [ "$#" -ge 20 ] || exit 124; shift 19; printf '%s' "$1") || exit 124
[ "$start" = "$2" ] || exit 124
kill -"$3" "$1" || exit "$?"
printf 'OPENSBX_SIGNAL %s %s %s\n' "$1" "$2" "$3"
)
`

// These variables are readonly, interpreted during shell startup, or rewritten
// at exec by common shells. Preserve them with direct exec rather than promising
// that an internal wrapper can restore their requested values exactly.
func shellSensitiveKey(key string) bool {
	if strings.HasPrefix(key, "BASH") && key != "BASH_ENV" || strings.HasPrefix(key, "ZSH") || strings.HasPrefix(key, "KSH") {
		return true
	}
	switch key {
	case "PWD", "SHLVL", "_", "SHELLOPTS", "UID", "EUID", "PPID", "RANDOM", "SECONDS", "LINENO", "OPTIND", "OPTARG", "DIRSTACK", "FUNCNAME", "GROUPS", "HISTCMD", "PIPESTATUS":
		return true
	}
	return false
}

func exportableKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Docker merges request overrides with Config.Env. Carry that effective original
// environment as data, never interpolated source, so shell-reset variables such
// as IFS and startup hooks can be restored without executing internal hooks.
func identityEnvironment(imageEnv []string, overrides map[string]string) ([]string, bool) {
	effective := make(map[string]string, len(imageEnv)+len(overrides))
	for _, entry := range imageEnv {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, false
		}
		effective[key] = value
	}
	for key, value := range overrides {
		effective[key] = value
	}
	keys := make([]string, 0, len(effective))
	for key := range effective {
		if !exportableKey(key) || shellSensitiveKey(key) {
			return nil, false
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+effective[key])
	}
	return pairs, true
}

func internalShellEnv(overrides []string) []string {
	result := make([]string, 0, len(overrides)+2)
	for _, entry := range overrides {
		key, _, _ := strings.Cut(entry, "=")
		if key != "ENV" && key != "BASH_ENV" {
			result = append(result, entry)
		}
	}
	return append(result, "ENV=", "BASH_ENV=")
}

func parseIdentity(record []byte, tag, nonce string) (int, uint64, error) {
	invalid := errors.New("invalid Docker guest identity preamble")
	if len(record) == 0 || len(record) > identityPreambleLimit || record[len(record)-1] != '\n' {
		return 0, 0, invalid
	}
	fields := strings.Split(string(record[:len(record)-1]), " ")
	if len(fields) != 4 || fields[0] != tag || fields[1] != nonce {
		return 0, 0, invalid
	}
	pid, pErr := strconv.ParseUint(fields[2], 10, 31)
	start, sErr := strconv.ParseUint(fields[3], 10, 64)
	if pErr != nil || sErr != nil || pid <= 1 || start == 0 || strconv.FormatUint(pid, 10) != fields[2] || strconv.FormatUint(start, 10) != fields[3] {
		return 0, 0, invalid
	}
	return int(pid), start, nil
}

type helperBuffer struct{ bytes.Buffer }

func (b *helperBuffer) Write(p []byte) (int, error) {
	if len(p) > identityPreambleLimit-b.Len() {
		return 0, errors.New("Docker identity helper exceeded its bounded output limit")
	}
	return b.Buffer.Write(p)
}

// Separate from filesystem execWithStdin: identity helpers have a fixed output
// bound and lifetime, including native create/attach/inspect and hijacked reads.
func (c *Client) runIdentityHelper(ctx context.Context, sandboxID string, opts moby.ExecCreateOptions) (execResult, error) {
	ctx, cancel := context.WithTimeout(ctx, identityHelperTimeout)
	defer cancel()
	opts.AttachStdout, opts.AttachStderr = true, true
	native, err := c.cli.ExecCreate(ctx, sandboxID, opts)
	if err != nil {
		return execResult{}, err
	}
	attached, err := c.cli.ExecAttach(ctx, native.ID, moby.ExecAttachOptions{})
	if err != nil {
		return execResult{}, err
	}
	defer attached.Close()
	stopClose := context.AfterFunc(ctx, attached.Close)
	defer stopClose()
	var stdout, stderr helperBuffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
	if ctx.Err() != nil {
		return execResult{}, ctx.Err()
	}
	if err != nil {
		return execResult{}, err
	}
	state, err := c.cli.ExecInspect(ctx, native.ID, moby.ExecInspectOptions{})
	if err != nil {
		return execResult{}, err
	}
	if ctx.Err() != nil {
		return execResult{}, ctx.Err()
	}
	if state.Running {
		return execResult{}, errors.New("Docker identity helper observation ended before confirmed completion")
	}
	return execResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: state.ExitCode}, nil
}

func missingIdentityShell(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	text := err.Error()
	// Do not treat arbitrary native errors, missing containers, permission errors,
	// or a timeout as evidence that a payload may safely be retried directly.
	return strings.Contains(text, `exec: "/bin/sh":`) && (strings.Contains(text, "no such file or directory") || strings.Contains(text, "executable file not found")) || strings.Contains(text, "exec /bin/sh: no such file or directory")
}

func (c *Client) probeIdentity(ctx context.Context, sandboxID, nonce, cwd string, env []string) (bool, error) {
	result, err := c.runIdentityHelper(ctx, sandboxID, moby.ExecCreateOptions{
		Cmd: []string{"/bin/sh", "-c", identityProbeScript, "opensbx-probe", nonce}, Env: internalShellEnv(env), WorkingDir: cwd,
	})
	if err != nil {
		if missingIdentityShell(err) {
			return false, nil
		}
		return false, err
	}
	if result.exitCode == 125 {
		return false, nil
	}
	if result.exitCode != 0 || result.stderr != "" {
		return false, errors.New("Docker guest identity capability probe failed")
	}
	if _, _, err := parseIdentity([]byte(result.stdout), "OPENSBX_PROBE", nonce); err != nil {
		return false, err
	}
	return true, nil
}

func (rc *runningCommand) publishIdentity(pid int, start uint64, err error) {
	rc.identityOnce.Do(func() {
		rc.mu.Lock()
		rc.guestPID, rc.guestStart, rc.identityErr = pid, start, err
		rc.mu.Unlock()
		close(rc.identityReady)
	})
}

// Filter exactly the first stderr record, never search later payload output.
// This writer is used only by the single demultiplexer goroutine.
type identityStderr struct {
	rc       *runningCommand
	nonce    string
	preamble []byte
	complete bool
}

func (w *identityStderr) Write(p []byte) (int, error) {
	if w.complete {
		return w.rc.stderr.Write(p)
	}
	end := bytes.IndexByte(p, '\n')
	prefix := len(p)
	if end >= 0 {
		prefix = end + 1
	}
	if prefix > identityPreambleLimit-len(w.preamble) || end < 0 && prefix == identityPreambleLimit-len(w.preamble) {
		err := errors.New("Docker guest identity preamble exceeded its bounded size")
		w.rc.publishIdentity(0, 0, err)
		return 0, err
	}
	w.preamble = append(w.preamble, p[:prefix]...)
	if end < 0 {
		return len(p), nil
	}
	pid, start, err := parseIdentity(w.preamble, "OPENSBX_EXEC", w.nonce)
	if err != nil {
		w.rc.publishIdentity(0, 0, err)
		return 0, err
	}
	w.complete, w.preamble = true, nil
	w.rc.publishIdentity(pid, start, nil)
	n, err := w.rc.stderr.Write(p[prefix:])
	return prefix + n, err
}

func (w *identityStderr) finish(err error) error {
	if !w.complete {
		if err == nil {
			err = errors.New("Docker guest identity preamble was not received before stderr closed")
		}
		w.rc.publishIdentity(0, 0, err)
	}
	return err
}

func (c *Client) waitGuestIdentity(ctx context.Context, rc *runningCommand) (int, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, identityHelperTimeout)
	defer cancel()
	if rc.identityReady == nil {
		return 0, 0, fmt.Errorf("%w: guest command identity is not retained", sandbox.ErrUnsupported)
	}
	select {
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case <-rc.identityReady:
	case <-rc.done:
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	if rc.identityErr != nil {
		return 0, 0, rc.identityErr
	}
	if rc.streamErr != nil {
		return 0, 0, rc.streamErr
	}
	if rc.finished {
		return 0, 0, sandbox.ErrCommandFinished
	}
	if rc.guestPID <= 1 || rc.guestStart == 0 {
		return 0, 0, errors.New("Docker guest command identity is not ready")
	}
	return rc.guestPID, rc.guestStart, nil
}

package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	domain "opensbx/internal/sandbox"
)

const commandLogLimit = 256 << 10
const maxTrackedCommands = 128

// The wrapper publishes its PID/starttime before the payload is allowed to
// start. exec replaces the wrapper without changing either. All user values
// are positional argv, never shell source. There is no host filesystem mount.
const commandWrapper = `
umask 077
dir=$1; token=$2; shift 2
mkdir "$dir" || exit 125
IFS= read -r stat < "/proc/$$/stat" || exit 125
rest=${stat##*) }
start=$(set -- $rest; shift 19; printf '%s' "$1")
case "$start" in ''|*[!0-9]*) exit 125;; esac
printf '%s %s %s\n' "$token" "$$" "$start" > "$dir/identity.tmp" || exit 125
mv "$dir/identity.tmp" "$dir/identity" || exit 125
IFS= read -r gate || exit 125
[ "$gate" = "$token" ] || exit 125
exec "$@"
`

// Numeric signals here are Linux guest signal numbers, not Darwin numbers.
// The /proc check narrows but cannot remove the check-to-kill kernel race.
const signalWrapper = `
pid=$1; expected=$2; signal=$3
case "$pid:$expected:$signal" in *[!0-9:]*) exit 125;; esac
[ "$pid" -gt 1 ] || exit 125
IFS= read -r stat < "/proc/$pid/stat" || exit 124
rest=${stat##*) }
actual=$(set -- $rest; shift 19; printf '%s' "$1")
[ "$actual" = "$expected" ] || exit 124
kill -"$signal" "$pid"
`

type runningCommand struct {
	sandboxID      string
	process        Process
	stdout, stderr *boundedBuffer
	done           chan struct{}
	mu             sync.Mutex
	detail         runtimeio.CommandDetail
	pid, start     string
	persistErr     error
}

func (r *runningCommand) snapshot() (runtimeio.CommandDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.detail
	d.Args = append([]string(nil), d.Args...)
	return d, r.persistErr
}
func commandDetail(row database.Command) runtimeio.CommandDetail {
	args := []string{}
	_ = json.Unmarshal([]byte(row.Args), &args)
	return runtimeio.CommandDetail{ID: row.ID, Name: row.Name, Args: args, Cwd: row.Cwd, SandboxID: row.SandboxID, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, ExitCode: row.ExitCode}
}
func (c *Client) commandRow(sandbox, id string) (*database.Command, error) {
	if _, err := c.owned(sandbox); err != nil {
		return nil, err
	}
	row, err := c.repo.FindCommandByID(id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.SandboxID != sandbox {
		return nil, domain.ErrCommandNotFound
	}
	return row, nil
}

func (c *Client) ExecCommand(ctx context.Context, sandbox string, req runtimeio.ExecCommandRequest) (runtimeio.CommandDetail, error) {
	if err := validateCommandInput(req); err != nil {
		return runtimeio.CommandDetail{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero runtimeio.CommandDetail
	if c.closing {
		return zero, errors.New("backend is shutting down")
	}
	if err := c.repo.RequireIdle("container", sandbox); err != nil {
		return zero, err
	}
	if err := c.running(ctx, sandbox); err != nil {
		return zero, err
	}
	if len(c.commands) >= maxTrackedCommands {
		// Evict only completed log buffers. Persistent metadata remains available.
		var oldest string
		var at int64
		for id, r := range c.commands {
			select {
			case <-r.done:
				d, _ := r.snapshot()
				if oldest == "" || d.StartedAt < at {
					oldest, at = id, d.StartedAt
				}
			default:
			}
		}
		if oldest == "" {
			return zero, errors.New("too many active commands (maximum 128)")
		}
		delete(c.commands, oldest)
	}
	args := []string{"exec", "--interactive"}
	if req.Cwd != "" {
		args = append(args, "--workdir="+req.Cwd)
	}
	envKeys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		args = append(args, "--env", key+"="+req.Env[key])
	}
	id, err := randomID("cmd_")
	if err != nil {
		return zero, err
	}
	dir := "/tmp/opensbx-" + id
	args = append(args, sandbox, "/bin/sh", "-c", commandWrapper, "opensbx-command", dir, id, req.Command)
	args = append(args, req.Args...)
	encoded, err := json.Marshal(req.Args)
	if err != nil {
		return zero, err
	}
	row := database.Command{ID: id, SandboxID: sandbox, Name: req.Command, Args: string(encoded), Cwd: req.Cwd, StartedAt: c.now().UnixMilli()}
	if err := c.repo.SaveCommand(row); err != nil {
		return zero, err
	}
	r := &runningCommand{sandboxID: sandbox, detail: commandDetail(row), done: make(chan struct{}), stdout: &boundedBuffer{limit: commandLogLimit, tail: true}, stderr: &boundedBuffer{limit: commandLogLimit, tail: true}}
	input, gate, err := os.Pipe()
	if err != nil {
		_ = c.repo.UpdateCommandFinished(id, 125, c.now().UnixMilli())
		return zero, errors.New("cannot create command input pipe")
	}
	p, err := c.runner.Start(args, input, r.stdout, r.stderr)
	if err != nil {
		_ = input.Close()
		_ = gate.Close()
		_ = c.repo.UpdateCommandFinished(id, 125, c.now().UnixMilli())
		return zero, safeError(err)
	}
	r.process = p
	c.commands[id] = r
	go func() {
		err := p.Wait()
		_ = input.Close()
		exit := 0
		if err != nil {
			exit = 125
			var e interface{ ExitCode() int }
			if errors.As(err, &e) {
				exit = e.ExitCode()
			}
		}
		at := c.now().UnixMilli()
		r.mu.Lock()
		r.detail.ExitCode = &exit
		r.detail.FinishedAt = &at
		r.persistErr = c.repo.UpdateCommandFinished(id, exit, at)
		r.mu.Unlock()
		close(r.done)
		// Exact generated directory only; never supplied by an API caller.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.lookup(cleanup, sandbox); err == nil {
			_, _ = c.run(cleanup, nil, "exec", sandbox, "/bin/sh", "-c", `rm -f "$1/identity" "$1/identity.tmp"; rmdir "$1"`, "opensbx-cleanup", dir)
		}
	}()
	// Handshake has its own bounded lifetime; disconnecting the HTTP caller
	// does not cancel an accepted guest command.
	ready, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer gate.Close()
	for {
		b, readErr := c.run(ready, nil, "exec", sandbox, "/bin/sh", "-c", `cat "$1/identity"`, "opensbx-identity", dir)
		if readErr == nil {
			fields := strings.Fields(string(b))
			if len(fields) != 3 || fields[0] != id {
				return zero, errors.New("invalid guest command identity")
			}
			pid, e1 := strconv.ParseUint(fields[1], 10, 32)
			start, e2 := strconv.ParseUint(fields[2], 10, 64)
			if e1 != nil || e2 != nil || pid <= 1 || start == 0 {
				return zero, errors.New("invalid guest PID identity")
			}
			r.pid, r.start = fields[1], fields[2]
			if _, err := io.WriteString(gate, id+"\n"); err != nil {
				return zero, errors.New("guest command handshake failed")
			}
			return r.snapshot()
		}
		select {
		case <-r.done:
			return r.snapshot()
		case <-ready.Done():
			_ = gate.Close()
			_ = p.Kill()
			return zero, errors.New("guest command handshake timed out; payload was not released")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (c *Client) GetCommand(ctx context.Context, sandbox, id string) (runtimeio.CommandDetail, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, err := c.commandRow(sandbox, id)
	if err != nil {
		return runtimeio.CommandDetail{}, err
	}
	if r := c.commands[id]; r != nil {
		return r.snapshot()
	}
	return commandDetail(*row), nil
}
func (c *Client) ListCommands(ctx context.Context, sandbox string) ([]runtimeio.CommandDetail, error) {
	if _, err := c.owned(sandbox); err != nil {
		return nil, err
	}
	rows, err := c.repo.FindCommandsBySandbox(sandbox)
	if err != nil {
		return nil, err
	}
	out := make([]runtimeio.CommandDetail, 0, len(rows))
	for _, row := range rows {
		out = append(out, commandDetail(row))
	}
	return out, nil
}
func (c *Client) KillCommand(ctx context.Context, sandbox, id string, signal int) (runtimeio.CommandDetail, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero runtimeio.CommandDetail
	row, err := c.commandRow(sandbox, id)
	if err != nil {
		return zero, err
	}
	if row.ExitCode != nil {
		return zero, domain.ErrCommandFinished
	}
	if signal < 1 || signal > 64 {
		return zero, errors.New("Linux signal must be between 1 and 64")
	}
	r := c.commands[id]
	if r == nil {
		return zero, errors.New("command is not tracked by this server instance")
	}
	select {
	case <-r.done:
		return zero, domain.ErrCommandFinished
	default:
	}
	if r.pid == "" || r.start == "" {
		return zero, errors.New("guest command identity is not ready")
	}
	if err := c.running(ctx, sandbox); err != nil {
		return zero, err
	}
	if _, err := c.run(ctx, nil, "exec", sandbox, "/bin/sh", "-c", signalWrapper, "opensbx-signal", r.pid, r.start, strconv.Itoa(signal)); err != nil {
		return zero, fmt.Errorf("guest command signal was not confirmed: %w", err)
	}
	return r.snapshot()
}
func (c *Client) tracked(sandbox, id string) (*runningCommand, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.commandRow(sandbox, id); err != nil {
		return nil, err
	}
	r := c.commands[id]
	if r == nil {
		return nil, errors.New("command logs/lifecycle are no longer retained in this server instance")
	}
	return r, nil
}
func (c *Client) WaitCommand(ctx context.Context, sandbox, id string) (runtimeio.CommandDetail, error) {
	r, err := c.tracked(sandbox, id)
	if err != nil {
		d, e := c.GetCommand(ctx, sandbox, id)
		if e == nil && d.ExitCode != nil {
			return d, nil
		}
		return runtimeio.CommandDetail{}, err
	}
	select {
	case <-ctx.Done():
		return runtimeio.CommandDetail{}, ctx.Err()
	case <-r.done:
		return r.snapshot()
	}
}
func (c *Client) GetCommandLogs(ctx context.Context, sandbox, id string) (runtimeio.CommandLogsResponse, error) {
	r, err := c.tracked(sandbox, id)
	if err != nil {
		return runtimeio.CommandLogsResponse{}, err
	}
	d, err := r.snapshot()
	if err != nil {
		return runtimeio.CommandLogsResponse{}, err
	}
	stdout, _ := r.stdout.snapshot()
	stderr, _ := r.stderr.snapshot()
	return runtimeio.CommandLogsResponse{Stdout: stdout, Stderr: stderr, ExitCode: d.ExitCode}, nil
}

type logReader struct {
	buffer *boundedBuffer
	done   <-chan struct{}
	closed chan struct{}
	once   sync.Once
	offset uint64
}

func (r *logReader) Close() error { r.once.Do(func() { close(r.closed) }); return nil }
func (r *logReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		select {
		case <-r.closed:
			return 0, io.ErrClosedPipe
		default:
		}
		r.buffer.mu.Lock()
		first := r.buffer.total - uint64(len(r.buffer.data))
		if r.offset < first {
			r.offset = first
		}
		n := copy(p, r.buffer.data[int(r.offset-first):])
		r.offset += uint64(n)
		r.buffer.mu.Unlock()
		if n > 0 {
			return n, nil
		}
		select {
		case <-r.closed:
			return 0, io.ErrClosedPipe
		case <-r.done:
			r.buffer.mu.Lock()
			remaining := r.offset < r.buffer.total
			r.buffer.mu.Unlock()
			if !remaining {
				return 0, io.EOF
			}
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func (c *Client) StreamCommandLogs(ctx context.Context, sandbox, id string) (io.ReadCloser, io.ReadCloser, error) {
	r, err := c.tracked(sandbox, id)
	if err != nil {
		return nil, nil, err
	}
	return &logReader{buffer: r.stdout, done: r.done, closed: make(chan struct{})}, &logReader{buffer: r.stderr, done: r.done, closed: make(chan struct{})}, nil
}

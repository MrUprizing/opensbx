package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/models"
)

// synchronizedRunner records argv safely across concurrent backend operations.
// It accepts only commands explicitly handled by the test callback.
type synchronizedRunner struct {
	t     *testing.T
	mu    sync.Mutex
	calls [][]string
	run   func(context.Context, []string, io.Reader, io.Writer, io.Writer) error
	start func([]string, io.Reader, io.Writer, io.Writer) (Process, error)
}

func (r *synchronizedRunner) Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	fn := r.run
	r.mu.Unlock()
	if fn == nil {
		r.t.Fatalf("unexpected CLI argv: %#v", args)
	}
	return fn(ctx, args, in, out, stderr)
}
func (r *synchronizedRunner) Start(args []string, in io.Reader, out, stderr io.Writer) (Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	fn := r.start
	r.mu.Unlock()
	if fn == nil {
		r.t.Fatalf("unexpected asynchronous CLI start: %#v", args)
		return nil, errors.New("unexpected Start")
	}
	return fn(args, in, out, stderr)
}
func (r *synchronizedRunner) count(args []string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if reflect.DeepEqual(call, args) {
			n++
		}
	}
	return n
}

func TestStatsForOneSandboxDoesNotBlockUnrelatedLifecycleOperation(t *testing.T) {
	a := "opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := "opensbx-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	entered := make(chan struct{})
	release := make(chan struct{})
	stopCompleted := make(chan struct{})
	var stateMu sync.Mutex
	state := map[string]string{a: "running", b: "running"}
	r := &synchronizedRunner{t: t}
	r.run = func(ctx context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			stateMu.Lock()
			sA, sB := state[a], state[b]
			stateMu.Unlock()
			_, _ = io.WriteString(out, "["+strings.TrimSuffix(strings.TrimPrefix(listJSON(a, sA, "[]"), "["), "]")+","+strings.TrimSuffix(strings.TrimPrefix(listJSON(b, sB, "[]"), "["), "]")+"]")
		case reflect.DeepEqual(args, []string{"stats", "--no-stream", "--format", "json", a}):
			select {
			case <-entered:
			default:
				close(entered)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, _ = io.WriteString(out, `[{"id":"`+a+`","cpuUsageUsec":2,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`)
		case reflect.DeepEqual(args, []string{"stats", "--no-stream", "--format", "json", b}):
			_, _ = io.WriteString(out, `[{"id":"`+b+`","cpuUsageUsec":2,"memoryUsageBytes":1,"memoryLimitBytes":2,"numProcesses":1}]`)
		case reflect.DeepEqual(args, []string{"stop", b}):
			stateMu.Lock()
			state[b] = "stopped"
			stateMu.Unlock()
			close(stopCompleted)
		default:
			return fmt.Errorf("unrecognized exact CLI argv: %#v", args)
		}
		return nil
	}
	c, repo := testClient(t, r)
	for _, id := range []string{a, b} {
		if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now()
	tick := 0
	clockMu := sync.Mutex{}
	c.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	statsDone := make(chan error, 1)
	go func() { _, err := c.Stats(context.Background(), a); statsDone <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Stats did not reach its blocked first sample")
	}
	stopDone := make(chan error, 1)
	go func() { err := c.Stop(context.Background(), b); stopDone <- err }()
	completedBeforeRelease := false
	select {
	case err := <-stopDone:
		completedBeforeRelease = err == nil
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-statsDone:
		if err != nil {
			t.Errorf("Stats() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stats did not finish after releasing sample")
	}
	if !completedBeforeRelease {
		select {
		case err := <-stopDone:
			if err != nil {
				t.Errorf("Stop(unrelated sandbox) = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("unrelated Stop remained blocked after stats release")
		}
		t.Fatal("Stats held the global lifecycle lock while waiting for CLI samples; unrelated Stop completed only after release")
	}
	select {
	case <-stopCompleted:
	default:
		t.Fatal("Stop did not reach the exact sandbox B CLI endpoint")
	}
}

func TestListUsesOneOwnedInventorySnapshotAndPreservesMissingRows(t *testing.T) {
	existing := "opensbx-cccccccccccccccccccccccccccccccc"
	missing := "opensbx-dddddddddddddddddddddddddddddddd"
	r := &synchronizedRunner{t: t, run: func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			return fmt.Errorf("unexpected list argv %#v", args)
		}
		_, _ = io.WriteString(out, listJSON(existing, "running", `[{"HostPort":23456,"ContainerPort":3000,"Proto":"tcp","Count":1}]`))
		return nil
	}}
	c, repo := testClient(t, r)
	for _, row := range []database.Sandbox{{ID: existing, Name: "present", Image: "node:24", Ports: database.JSONMap{"3000/tcp": "23456"}}, {ID: missing, Name: "absent", Image: "node:20", Ports: database.JSONMap{"8080/tcp": "28080"}}} {
		if err := repo.Save(row); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List() returned %d entries, want both repository rows: %+v", len(got), got)
	}
	byID := map[string]struct {
		state, name string
		ports       []string
	}{}
	for _, item := range got {
		byID[item.ID] = struct {
			state, name string
			ports       []string
		}{item.State, item.Name, item.Ports}
	}
	if item := byID[existing]; item.state != "running" || item.name != "present" || !reflect.DeepEqual(item.ports, []string{"3000/tcp"}) {
		t.Errorf("owned live entry = %+v", item)
	}
	if item := byID[missing]; item.state != "removed" || item.name != "absent" || !reflect.DeepEqual(item.ports, []string{"8080/tcp"}) {
		t.Errorf("missing entry = %+v", item)
	}
	if got := r.count([]string{"list", "--all", "--format", "json"}); got != 1 {
		t.Errorf("full inventory CLI calls = %d, want one shared snapshot", got)
	}
	for _, id := range []string{existing, missing} {
		row, err := repo.FindByID(id)
		if err != nil || row == nil {
			t.Errorf("List mutated/deleted persisted row %s: row=%+v err=%v", id, row, err)
		}
	}
}

func TestListReturnsOwnershipErrorForRepositoryIDClaimedByDifferentContainer(t *testing.T) {
	id := "opensbx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	r := &synchronizedRunner{t: t, run: func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			return fmt.Errorf("unexpected argv %#v", args)
		}
		payload := listJSON(id, "running", "[]")
		payload = strings.Replace(payload, `"io.opensbx.managed":"`+id+`"`, `"io.opensbx.managed":"some-other-owner"`, 1)
		_, _ = io.WriteString(out, payload)
		return nil
	}}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "ownership label") {
		t.Fatalf("List() ownership mismatch error = %v", err)
	}
	if r.count([]string{"list", "--all", "--format", "json"}) != 1 {
		t.Fatal("ownership check did not use one inventory snapshot")
	}
}

func TestCommandTrackingRejects129thActiveCommand(t *testing.T) {
	id := "opensbx-12121212121212121212121212121212"
	r := &synchronizedRunner{t: t, run: func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			return fmt.Errorf("unexpected argv %#v", args)
		}
		_, _ = io.WriteString(out, listJSON(id, "running", "[]"))
		return nil
	}}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < maxTrackedCommands; n++ {
		commandID := fmt.Sprintf("cmd_%03d", n)
		c.commands[commandID] = &runningCommand{sandboxID: id, done: make(chan struct{}), detail: models.CommandDetail{ID: commandID, StartedAt: int64(n)}}
	}
	if _, err := c.ExecCommand(context.Background(), id, models.ExecCommandRequest{Command: "echo"}); err == nil || !strings.Contains(err.Error(), "maximum 128") {
		t.Fatalf("129th active command error = %v", err)
	}
	if len(c.commands) != maxTrackedCommands {
		t.Fatalf("active tracked command count changed to %d", len(c.commands))
	}
}

func TestCommandLogAPIReturnsNewest256KiBPerStream(t *testing.T) {
	id := "opensbx-13131313131313131313131313131313"
	commandID := "cmd_bounded"
	c, repo := testClient(t, &synchronizedRunner{t: t})
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCommand(database.Command{ID: commandID, SandboxID: id, Name: "writer", Args: "[]"}); err != nil {
		t.Fatal(err)
	}
	stdout := &boundedBuffer{limit: commandLogLimit, tail: true}
	stderr := &boundedBuffer{limit: commandLogLimit, tail: true}
	_, _ = io.WriteString(stdout, "discarded"+strings.Repeat("o", commandLogLimit-4)+"TAIL")
	_, _ = io.WriteString(stderr, "discarded"+strings.Repeat("e", commandLogLimit-4)+"LAST")
	done := make(chan struct{})
	close(done)
	c.commands[commandID] = &runningCommand{sandboxID: id, detail: models.CommandDetail{ID: commandID}, stdout: stdout, stderr: stderr, done: done}
	logs, err := c.GetCommandLogs(context.Background(), id, commandID)
	wantOut := strings.Repeat("o", commandLogLimit-4) + "TAIL"
	wantErr := strings.Repeat("e", commandLogLimit-4) + "LAST"
	if err != nil || logs.Stdout != wantOut || logs.Stderr != wantErr {
		t.Fatalf("bounded command logs lengths=%d/%d err=%v", len(logs.Stdout), len(logs.Stderr), err)
	}
	outReader, errReader, err := c.StreamCommandLogs(context.Background(), id, commandID)
	if err != nil {
		t.Fatal(err)
	}
	defer outReader.Close()
	defer errReader.Close()
	outBytes, err := io.ReadAll(outReader)
	if err != nil || string(outBytes) != wantOut {
		t.Fatalf("bounded stdout stream length=%d err=%v", len(outBytes), err)
	}
	errBytes, err := io.ReadAll(errReader)
	if err != nil || string(errBytes) != wantErr {
		t.Fatalf("bounded stderr stream length=%d err=%v", len(errBytes), err)
	}
}

func TestRemoveKillsAttachedCLIWaitAndDoesNotRunGuestCleanupAfterSandboxDeletion(t *testing.T) {
	const sandbox = "opensbx-14141414141414141414141414141414"
	r, process, cleanup := activeCommandRunner(t, sandbox)
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox}); err != nil {
		t.Fatal(err)
	}
	command, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "sleep"})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	tracked := c.commands[command.ID]
	c.mu.Unlock()
	if err := c.Remove(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	if !process.isKilled() {
		t.Fatal("Remove did not kill the attached CLI process after deleting its guest")
	}
	select {
	case <-tracked.done:
	case <-time.After(2 * time.Second):
		t.Fatal("attached process Wait did not complete after Remove")
	}
	select {
	case <-cleanup:
		t.Fatal("guest cleanup exec ran after the sandbox had already been deleted")
	default:
	}
	row, err := repo.FindByID(sandbox)
	if err != nil || row != nil {
		t.Fatalf("sandbox row after Remove = %+v, %v", row, err)
	}
}

func TestShutdownStopsSandboxKillsAttachedCLIAndWaitCleanupCompletes(t *testing.T) {
	const sandbox = "opensbx-15151515151515151515151515151515"
	r, process, cleanup := activeCommandRunner(t, sandbox)
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox}); err != nil {
		t.Fatal(err)
	}
	command, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "sleep"})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	tracked := c.commands[command.ID]
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.Shutdown(ctx)
	if !process.isKilled() {
		t.Fatal("Shutdown did not kill attached CLI process")
	}
	select {
	case <-tracked.done:
	case <-time.After(2 * time.Second):
		t.Fatal("attached command Wait did not finish on Shutdown")
	}
	select {
	case <-cleanup:
	case <-time.After(2 * time.Second):
		t.Fatal("command exit did not clean its exact guest identity directory")
	}
}

func activeCommandRunner(t *testing.T, sandbox string) (*synchronizedRunner, *controlledProcess, chan struct{}) {
	t.Helper()
	process := newControlledProcess()
	cleanup := make(chan struct{}, 1)
	tokenToPID := map[string]string{}
	var stateMu sync.Mutex
	state := "running"
	r := &synchronizedRunner{t: t}
	r.run = func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			stateMu.Lock()
			current := state
			stateMu.Unlock()
			_, _ = io.WriteString(out, listJSON(sandbox, current, "[]"))
		case len(args) == 7 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == `cat "$1/identity"` && args[5] == "opensbx-identity":
			dir := args[6]
			for token, pid := range tokenToPID {
				if strings.HasSuffix(dir, token) {
					_, _ = fmt.Fprintf(out, "%s %s 12345\n", token, pid)
					return nil
				}
			}
			return fmt.Errorf("unknown command identity dir %q", dir)
		case len(args) == 3 && args[0] == "delete" && args[1] == "--force" && args[2] == sandbox:
		case reflect.DeepEqual(args, []string{"stop", sandbox}):
			stateMu.Lock()
			state = "stopped"
			stateMu.Unlock()
		case len(args) == 7 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == `rm -f "$1/identity" "$1/identity.tmp"; rmdir "$1"` && args[5] == "opensbx-cleanup":
			cleanup <- struct{}{}
		default:
			return fmt.Errorf("unknown command lifecycle argv %#v", args)
		}
		return nil
	}
	r.start = func(args []string, _ io.Reader, _ io.Writer, _ io.Writer) (Process, error) {
		if len(args) != 10 || args[0] != "exec" || args[1] != "--interactive" || args[2] != sandbox || args[5] != commandWrapper || args[6] != "opensbx-command" {
			return nil, fmt.Errorf("unexpected attached command argv %#v", args)
		}
		tokenToPID[args[8]] = "321"
		return process, nil
	}
	return r, process, cleanup
}

type exitStatusError int

func (e exitStatusError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatusError) ExitCode() int { return int(e) }

type immediateProcess struct{ err error }

func (p immediateProcess) Wait() error { return p.err }
func (immediateProcess) Kill() error   { return nil }

func commandHandshakeRunner(t *testing.T, sandbox, mode string) (*synchronizedRunner, *controlledProcess) {
	t.Helper()
	process := newControlledProcess()
	identityDir := ""
	token := ""
	r := &synchronizedRunner{t: t}
	r.run = func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(sandbox, "running", "[]"))
			return nil
		case len(args) == 7 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == `cat "$1/identity"` && args[5] == "opensbx-identity":
			if args[6] != identityDir {
				return fmt.Errorf("identity script used unexpected directory %q", args[6])
			}
			switch mode {
			case "invalid":
				_, _ = io.WriteString(out, "wrong-command-token 300 55\n")
				return nil
			case "early-exit", "timeout":
				return errors.New("identity file not yet visible")
			default:
				_, _ = fmt.Fprintf(out, "%s 300 55\n", token)
				return nil
			}
		case len(args) == 7 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == `rm -f "$1/identity" "$1/identity.tmp"; rmdir "$1"` && args[5] == "opensbx-cleanup":
			return nil
		default:
			return fmt.Errorf("unrecognized handshake argv %#v", args)
		}
	}
	r.start = func(args []string, _ io.Reader, _ io.Writer, _ io.Writer) (Process, error) {
		if len(args) != 10 || args[0] != "exec" || args[1] != "--interactive" || args[2] != sandbox || args[3] != "/bin/sh" || args[4] != "-c" || args[5] != commandWrapper || args[6] != "opensbx-command" {
			return nil, fmt.Errorf("unrecognized command start argv %#v", args)
		}
		identityDir, token = args[7], args[8]
		if mode == "timeout" {
			return process, nil
		}
		if mode == "early-exit" {
			return immediateProcess{err: exitStatusError(17)}, nil
		}
		return process, nil
	}
	return r, process
}

func TestExecCommandRejectsInvalidGuestIdentity(t *testing.T) {
	const sandbox = "opensbx-16161616161616161616161616161616"
	r, process := commandHandshakeRunner(t, sandbox, "invalid")
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "echo"}); err == nil || !strings.Contains(err.Error(), "invalid guest command identity") {
		t.Fatalf("invalid handshake identity error = %v", err)
	}
	_ = process.Kill()
}

func TestExecCommandReturnsEarlyGuestExitWithoutWaitingForHandshakeTimeout(t *testing.T) {
	const sandbox = "opensbx-17171717171717171717171717171717"
	r, _ := commandHandshakeRunner(t, sandbox, "early-exit")
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	detail, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "exit"})
	if err != nil || detail.ExitCode == nil || *detail.ExitCode != 17 {
		t.Fatalf("early exit result=%+v err=%v", detail, err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("completed guest process waited for the full command identity timeout")
	}
}

func TestExecCommandHandshakeTimeoutKillsAttachedCLIWithoutReleasingPayload(t *testing.T) {
	const sandbox = "opensbx-18181818181818181818181818181818"
	r, process := commandHandshakeRunner(t, sandbox, "timeout")
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox}); err != nil {
		t.Fatal(err)
	}
	errDone := make(chan error, 1)
	go func() {
		_, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "sleep"})
		errDone <- err
	}()
	select {
	case err := <-errDone:
		if err == nil || !strings.Contains(err.Error(), "handshake timed out") {
			t.Fatalf("handshake result = %v", err)
		}
		if !process.isKilled() {
			t.Fatal("handshake timeout did not kill attached CLI")
		}
	case <-time.After(12 * time.Second):
		t.Fatal("command identity handshake exceeded its bounded 10-second timeout")
	}
}

func isolatedFileScriptRunner(t *testing.T, id, root string) *synchronizedRunner {
	t.Helper()
	r := &synchronizedRunner{t: t}
	r.run = func(_ context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(id, "running", "[]"))
			return nil
		case len(args) == 8 && args[0] == "exec" && args[1] == "--interactive" && args[2] == id && args[3] == "/bin/sh" && args[4] == "-c" && args[6] == "opensbx-file":
			path := args[7]
			if filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
				return fmt.Errorf("test runner rejected non-relative path %q", path)
			}
			resolved := filepath.Clean(filepath.Join(root, path))
			rel, err := filepath.Rel(root, resolved)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("test runner refused path outside TempDir: %q", path)
			}
			allowed := map[string]bool{
				`case "$1" in /*) ;; *) set -- "./$1";; esac; cat > "$1"`:                               true,
				`case "$1" in /*) ;; *) set -- "./$1";; esac; mkdir -p "$(dirname "$1")"; cat > "$1"`:   true,
				`case "$1" in /*) ;; *) set -- "./$1";; esac; mkdir -p "$(dirname "$1")" && cat > "$1"`: true,
				`case "$1" in /*) ;; *) set -- "./$1";; esac; exec cat "$1"`:                            true,
				`case "$1" in /*) ;; *) set -- "./$1";; esac; exec rm "$1"`:                             true,
				`case "$1" in /*) ;; *) set -- "./$1";; esac; exec rm -rf "$1"`:                         true,
				`case "$1" in /*) ;; *) set -- "./$1";; esac; exec rm -rf -- "$1"`:                      true,
			}
			if !allowed[args[5]] {
				return fmt.Errorf("test runner refuses unknown guest script %q", args[5])
			}
			cmd := exec.Command("/bin/sh", "-c", args[5], args[6], path)
			cmd.Dir = root
			cmd.Stdin = in
			cmd.Stdout = out
			cmd.Stderr = stderr
			return cmd.Run()
		default:
			return fmt.Errorf("test runner rejects argv outside file API: %#v", args)
		}
	}
	return r
}

func TestFileOperationScriptsCreateParentsAndTreatPathsAsLiteralArguments(t *testing.T) {
	id := "opensbx-ffffffffffffffffffffffffffffffff"
	root := t.TempDir()
	r := isolatedFileScriptRunner(t, id, root)
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	relative := "nested/space/-literal; $(touch shell-injected)"
	want := "created parents safely"
	if err := c.WriteFile(context.Background(), id, relative, want); err != nil {
		t.Fatalf("WriteFile nested path should create parent directories: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, relative)); err != nil || string(got) != want {
		t.Fatalf("literal relative file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "shell-injected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guest path was interpreted as shell source inside the isolated TempDir: %v", err)
	}
	read, err := c.ReadFile(context.Background(), id, relative)
	if err != nil || read != want {
		t.Fatalf("ReadFile literal relative path = %q, %v", read, err)
	}
	leadingDash := "-leading dash; name"
	if err := c.WriteFile(context.Background(), id, leadingDash, "dash-safe"); err != nil {
		t.Fatalf("WriteFile leading dash path: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, leadingDash)); err != nil || string(got) != "dash-safe" {
		t.Fatalf("leading dash file = %q, %v", got, err)
	}
}

func TestDeleteFileRecursivelyRemovesNestedTreeAndTreatsAbsenceAsSuccess(t *testing.T) {
	id := "opensbx-ffffffffffffffffffffffffffffffff"
	root := t.TempDir()
	deleteTree := "nested delete/tree"
	if err := os.MkdirAll(filepath.Join(root, deleteTree, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, deleteTree, "child", "entry"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	r := isolatedFileScriptRunner(t, id, root)
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteFile(context.Background(), id, deleteTree); err != nil {
		t.Fatalf("DeleteFile should recursively remove the requested nonempty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, deleteTree)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted directory still exists or has unexpected error: %v", err)
	}
	if err := c.DeleteFile(context.Background(), id, deleteTree); err != nil {
		t.Fatalf("DeleteFile of an already absent path should succeed: %v", err)
	}
}

func TestDeleteFileOfAlreadyAbsentPathIsSuccessful(t *testing.T) {
	const id = "opensbx-ffffffffffffffffffffffffffffffff"
	root := t.TempDir()
	r := isolatedFileScriptRunner(t, id, root)
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	err := c.DeleteFile(context.Background(), id, "missing parent/missing file")
	if err != nil {
		t.Fatalf("DeleteFile of an absent path should be idempotent: %v", err)
	}
}

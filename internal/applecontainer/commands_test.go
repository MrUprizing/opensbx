package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"opensbx/internal/database"
	"opensbx/models"
)

type controlledProcess struct {
	mu       sync.Mutex
	finished chan struct{}
	killed   bool
}

func newControlledProcess() *controlledProcess {
	return &controlledProcess{finished: make(chan struct{})}
}
func (p *controlledProcess) Wait() error { <-p.finished; return nil }
func (p *controlledProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.killed {
		p.killed = true
		close(p.finished)
	}
	return nil
}
func (p *controlledProcess) isKilled() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.killed }

func TestKillCommandTargetsOnlySelectedIdenticalCommandAndUsesGuestIdentity(t *testing.T) {
	sandbox := "opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := &scriptedRunner{t: t}
	processes := map[string]*controlledProcess{}
	processByPID := map[string]*controlledProcess{}
	pidByToken := map[string]string{}
	r.startFn = func(args []string, _ io.Reader, stdout, stderr io.Writer) (Process, error) {
		if len(args) != 11 || args[0] != "exec" || args[1] != "--interactive" || args[2] != sandbox || args[3] != "/bin/sh" || args[4] != "-c" || args[5] != commandWrapper || args[6] != "opensbx-command" {
			return nil, fmt.Errorf("unexpected attached exec argv: %#v", args)
		}
		if args[9] != "sleep" || args[10] != "same argument" {
			return nil, fmt.Errorf("payload argv was not positional: %#v", args)
		}
		token := args[8]
		if strings.Contains(args[7], token) == false || strings.Contains(args[5], token) {
			return nil, fmt.Errorf("unsafe wrapper token placement: %#v", args)
		}
		pid := strconv.Itoa(300 + len(processes))
		p := newControlledProcess()
		_, _ = io.WriteString(stdout, "guest stdout")
		_, _ = io.WriteString(stderr, "guest stderr")
		processes[token] = p
		pidByToken[token] = pid
		processByPID[pid] = p
		return p, nil
	}
	r.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			_, _ = io.WriteString(out, listJSON(sandbox, "running", "[]"))
		case len(args) == 7 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == `cat "$1/identity"` && args[5] == "opensbx-identity":
			dir := args[6]
			for token, pid := range pidByToken {
				if strings.HasSuffix(dir, token) {
					_, _ = fmt.Fprintf(out, "%s %s 987654\n", token, pid)
					return nil
				}
			}
			return fmt.Errorf("identity read used unknown command directory %q", dir)
		case len(args) == 9 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == signalWrapper && args[5] == "opensbx-signal":
			pid, start, signal := args[6], args[7], args[8]
			if start != "987654" || signal != "9" {
				return fmt.Errorf("signal identity not positional/exact: %#v", args)
			}
			if p := processByPID[pid]; p != nil {
				return p.Kill()
			}
			return fmt.Errorf("PID %s did not match an attached guest process", pid)
		case len(args) == 7 && args[0] == "exec" && args[1] == sandbox && args[2] == "/bin/sh" && args[3] == "-c" && args[4] == `rm -f "$1/identity" "$1/identity.tmp"; rmdir "$1"` && args[5] == "opensbx-cleanup":
			return nil
		default:
			return fmt.Errorf("unrecognized exact CLI argv: %#v", args)
		}
		return nil
	}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox, Image: "node:24"}); err != nil {
		t.Fatal(err)
	}
	first, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "sleep", Args: []string{"same argument"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.ExecCommand(context.Background(), sandbox, models.ExecCommandRequest{Command: "sleep", Args: []string{"same argument"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("identical commands shared ID %q", first.ID)
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := c.WaitCommand(waitCtx, sandbox, first.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait error = %v", err)
	}
	if processes[first.ID].isKilled() {
		t.Fatal("canceling WaitCommand killed the accepted command")
	}
	if _, err := c.KillCommand(context.Background(), sandbox, second.ID, 9); err != nil {
		t.Fatal(err)
	}
	if !processes[second.ID].isKilled() {
		t.Fatal("selected command was not terminated")
	}
	if processes[first.ID].isKilled() {
		t.Fatal("sibling identical command was terminated")
	}
	if _, err := c.KillCommand(context.Background(), "opensbx-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", first.ID, 9); err == nil {
		t.Fatal("cross-sandbox command kill accepted")
	}
	if processes[first.ID].isKilled() {
		t.Fatal("cross-sandbox rejection affected sibling process")
	}
	if _, err := c.WaitCommand(context.Background(), sandbox, second.ID); err != nil {
		t.Fatalf("WaitCommand after SIGKILL = %v", err)
	}
	detail, err := c.GetCommand(context.Background(), sandbox, second.ID)
	if err != nil || detail.ExitCode == nil || *detail.ExitCode != 0 {
		t.Fatalf("GetCommand() = %+v, %v", detail, err)
	}
	commands, err := c.ListCommands(context.Background(), sandbox)
	if err != nil || len(commands) != 2 {
		t.Fatalf("ListCommands() = %+v, %v", commands, err)
	}
	logs, err := c.GetCommandLogs(context.Background(), sandbox, second.ID)
	if err != nil || logs.Stdout != "guest stdout" || logs.Stderr != "guest stderr" {
		t.Fatalf("GetCommandLogs() = %+v, %v", logs, err)
	}
	stdout, stderr, err := c.StreamCommandLogs(context.Background(), sandbox, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer stderr.Close()
	outBytes, err := io.ReadAll(stdout)
	if err != nil || string(outBytes) != "guest stdout" {
		t.Fatalf("stdout stream = %q, %v", outBytes, err)
	}
	errBytes, err := io.ReadAll(stderr)
	if err != nil || string(errBytes) != "guest stderr" {
		t.Fatalf("stderr stream = %q, %v", errBytes, err)
	}
}

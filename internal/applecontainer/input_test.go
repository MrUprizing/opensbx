package applecontainer

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
)

func TestExecInputBudgetIncludesAllFieldsAtExactByteAndEntryBoundaries(t *testing.T) {
	tests := []struct {
		name string
		req  runtimeio.ExecCommandRequest
		ok   bool
	}{
		{name: "one executable consumes exact byte limit", req: runtimeio.ExecCommandRequest{Command: strings.Repeat("x", maxInputBytes-1)}, ok: true},
		{name: "combined executable and argument consume exact byte limit", req: runtimeio.ExecCommandRequest{Command: strings.Repeat("c", 40000), Args: []string{strings.Repeat("a", maxInputBytes-40002)}}, ok: true},
		{name: "combined executable and argument exceed byte limit", req: runtimeio.ExecCommandRequest{Command: strings.Repeat("c", 40000), Args: []string{strings.Repeat("a", maxInputBytes-40001)}}, ok: false},
		{name: "command cwd and environment share byte budget", req: runtimeio.ExecCommandRequest{Command: strings.Repeat("c", 40000), Cwd: strings.Repeat("d", 25531), Env: map[string]string{"K": "v"}}, ok: false},
		{name: "1024 entries including executable", req: runtimeio.ExecCommandRequest{Command: "x", Args: make([]string, maxInputEntries-1)}, ok: true},
		{name: "1025 entries including executable", req: runtimeio.ExecCommandRequest{Command: "x", Args: make([]string, maxInputEntries)}, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommandBudget(tc.req)
			if (err == nil) != tc.ok {
				t.Fatalf("validateCommandBudget() error = %v, want accepted=%v", err, tc.ok)
			}
		})
	}
}

func TestCreateEnvironmentBudgetHasIndependentExactByteAndEntryLimits(t *testing.T) {
	within := "K=" + strings.Repeat("v", maxInputBytes-3) // value plus '=' and trailing NUL: exactly 64 KiB.
	if _, err := envArgs([]string{within}); err != nil {
		t.Fatalf("creation environment at byte limit rejected: %v", err)
	}
	if _, err := envArgs([]string{"K=" + strings.Repeat("v", maxInputBytes-2)}); err == nil {
		t.Fatal("creation environment over byte limit accepted")
	}
	atLimit := make([]string, maxInputEntries)
	for i := range atLimit {
		atLimit[i] = fmt.Sprintf("K%d=x", i)
	}
	if _, err := envArgs(atLimit); err != nil {
		t.Fatalf("creation environment at entry limit rejected: %v", err)
	}
	if _, err := envArgs(append(atLimit, "EXTRA=x")); err == nil {
		t.Fatal("creation environment over entry limit accepted")
	}
}

func TestOversizedCreateInputIsRejectedBeforeCLIOrPersistence(t *testing.T) {
	r := &scriptedRunner{t: t}
	c, repo := testClient(t, r)
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "combined creation env byte limit", env: []string{"K=" + strings.Repeat("v", maxInputBytes-2)}},
		{name: "oversized creation env key", env: []string{strings.Repeat("K", maxInputBytes-1) + "=x"}},
		{name: "oversized creation env value", env: []string{"K=" + strings.Repeat("v", maxInputBytes)}},
		{name: "creation env entry limit", env: func() []string {
			out := make([]string, maxInputEntries+1)
			for i := range out {
				out[i] = fmt.Sprintf("K%d=x", i)
			}
			return out
		}()},
		{name: "nul creation env key", env: []string{"BAD\x00KEY=value"}},
		{name: "nul creation env value", env: []string{"KEY=bad\x00value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Create(context.Background(), runtimeio.CreateSandboxRequest{Image: "node:24", Env: tc.env})
			if err == nil {
				t.Fatal("oversized create environment accepted")
			}
			if len(r.calls) != 0 {
				t.Fatalf("invalid create environment reached Apple CLI: %#v", r.calls)
			}
			rows, findErr := repo.FindAll()
			if findErr != nil || len(rows) != 0 {
				t.Fatalf("invalid create environment persisted rows=%+v err=%v", rows, findErr)
			}
		})
	}
}

func TestExecInputBudgetRejectionsPrecedeCLICommandPersistenceAndLaunch(t *testing.T) {
	const sandbox = "opensbx-20202020202020202020202020202020"
	r := &scriptedRunner{t: t, run: func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}) {
			return fmt.Errorf("unexpected CLI argv %#v", args)
		}
		_, _ = io.WriteString(out, listJSON(sandbox, "running", "[]"))
		return nil
	}}
	c, repo := testClient(t, r)
	if err := repo.Save(database.Sandbox{ID: sandbox, Name: sandbox}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		req       runtimeio.ExecCommandRequest
		wantCalls int
	}{
		{name: "combined exec byte overflow", req: runtimeio.ExecCommandRequest{Command: strings.Repeat("c", 40000), Args: []string{strings.Repeat("a", maxInputBytes-40001)}}},
		{name: "oversized exec env key", req: runtimeio.ExecCommandRequest{Command: "echo", Env: map[string]string{strings.Repeat("K", maxInputBytes): "x"}}},
		{name: "oversized exec env value", req: runtimeio.ExecCommandRequest{Command: "echo", Env: map[string]string{"K": strings.Repeat("v", maxInputBytes)}}},
		{name: "exec entry overflow", req: runtimeio.ExecCommandRequest{Command: "echo", Args: make([]string, maxInputEntries)}},
		{name: "nul executable", req: runtimeio.ExecCommandRequest{Command: "ec\x00ho"}},
		{name: "nul argument", req: runtimeio.ExecCommandRequest{Command: "echo", Args: []string{"bad\x00arg"}}},
		{name: "nul working directory", req: runtimeio.ExecCommandRequest{Command: "echo", Cwd: "/tmp/bad\x00cwd"}},
		{name: "nul environment key", req: runtimeio.ExecCommandRequest{Command: "echo", Env: map[string]string{"BAD\x00KEY": "value"}}},
		{name: "nul environment value", req: runtimeio.ExecCommandRequest{Command: "echo", Env: map[string]string{"KEY": "bad\x00value"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeCalls := len(r.calls)
			beforeRows, err := repo.FindCommandsBySandbox(sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.ExecCommand(context.Background(), sandbox, tc.req); err == nil {
				t.Fatal("invalid or oversized command input accepted")
			}
			if got := len(r.calls) - beforeCalls; got != tc.wantCalls {
				t.Errorf("invalid input made %d CLI calls, want %d", got, tc.wantCalls)
			}
			afterRows, err := repo.FindCommandsBySandbox(sandbox)
			if err != nil || len(afterRows) != len(beforeRows) {
				t.Errorf("invalid input persisted command history: before=%d after=%d err=%v", len(beforeRows), len(afterRows), err)
			}
		})
	}
}

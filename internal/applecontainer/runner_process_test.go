package applecontainer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeExecutableFixture(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the native CLI process fixture uses a POSIX shell shebang")
	}
	path := filepath.Join(t.TempDir(), "fixture-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRunnerForwardsInputAndCapturesOutputWithoutShellReconstruction(t *testing.T) {
	runner := cliRunner{path: writeExecutableFixture(t, "IFS= read -r line; printf 'out:%s' \"$line\"; printf 'err:%s' \"$1\" >&2\n")}
	var stdout, stderr bytes.Buffer
	if err := runner.Run(context.Background(), []string{"arg with spaces"}, bytes.NewBufferString("literal input\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out:literal input" || stderr.String() != "err:arg with spaces" {
		t.Fatalf("runner stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCLIRunnerStartWaitAndKillTrackTheStartedProcess(t *testing.T) {
	short := cliRunner{path: writeExecutableFixture(t, "printf 'finished'\n")}
	process, err := short.Start([]string{"start"}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("Wait() on successful process: %v", err)
	}

	sleeper := cliRunner{path: writeExecutableFixture(t, "exec /bin/sleep 30\n")}
	process, err = sleeper.Start(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill(): %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- process.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("Wait() after Kill() reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("killed CLI process did not terminate")
	}
}

func TestCLIRunnerRunReturnsCallerCancellation(t *testing.T) {
	runner := cliRunner{path: writeExecutableFixture(t, "exec /bin/sleep 30\n")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx, nil, nil, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() canceled error=%v", err)
	}
}

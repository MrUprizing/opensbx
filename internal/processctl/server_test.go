package processctl

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServerProcessHelper(t *testing.T) {
	if os.Getenv("OPENSBX_PROCESSCTL_HELPER") != "1" {
		return
	}
	dir := os.Getenv("OPENSBX_PROCESSCTL_DIR")
	guard, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire child server lock: %v", err)
	}
	if err := guard.Publish(os.Getpid()); err != nil {
		t.Fatalf("publish child server PID: %v", err)
	}
	ctx, stop := signal.NotifyContext(t.Context(), processTerminationSignals()...)
	defer stop()
	<-ctx.Done()
	if err := guard.Close(); err != nil {
		t.Fatalf("release child server lock: %v", err)
	}
}

func TestStopGracefullySignalsLockedServerAndWaits(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServerProcessHelper$")
	configureTestServerCommand(cmd)
	cmd.Env = append(os.Environ(), "OPENSBX_PROCESSCTL_HELPER=1", "OPENSBX_PROCESSCTL_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		pid, running, err := Running(dir)
		if err != nil {
			t.Fatal(err)
		}
		if running && pid > 0 {
			childPID = pid
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("server helper exited before publishing its PID: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		_ = cmd.Process.Kill()
		<-finished
		t.Fatal("server helper did not publish its PID")
	}

	stoppedPID, err := Stop(dir, 5*time.Second)
	if err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if stoppedPID != childPID {
		t.Fatalf("Stop() PID=%d, want %d", stoppedPID, childPID)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("server helper exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("server helper did not exit after SIGTERM")
	}
	if _, running, err := Running(dir); err != nil || running {
		t.Fatalf("Running() after stop = (%v, %v), want not running", running, err)
	}
}

func TestRunningRemovesStalePIDRecord(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, pidFileName)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, running, err := Running(dir); err != nil || running {
		t.Fatalf("Running() = (%v, %v), want not running", running, err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("stale PID record stat error = %v, want removed", err)
	}
	if _, err := Stop(dir, time.Second); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Stop() on inactive server error = %v", err)
	}
}

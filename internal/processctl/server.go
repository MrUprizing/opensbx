package processctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

var ErrNotRunning = errors.New("OpenSBX is not running")

const (
	lockFileName = "opensbx.lock"
	pidFileName  = "opensbx.pid"
)

type ServerLock struct {
	lock    *flock.Flock
	pidPath string
}

// Acquire holds a per-data-directory lock for the lifetime of a server.
func Acquire(dataDir string) (*ServerLock, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare server control directory: %w", err)
	}
	path := filepath.Join(dir, lockFileName)
	if file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, fmt.Errorf("open server control lock: %w", err)
	} else {
		_ = file.Close()
	}
	guard := flock.New(path)
	acquired, err := guard.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock server control directory: %w", err)
	}
	if !acquired {
		pid, _ := readPID(filepath.Join(dir, pidFileName))
		if pid > 0 {
			return nil, fmt.Errorf("OpenSBX is already running (PID %d)", pid)
		}
		return nil, errors.New("OpenSBX is already starting or running")
	}
	pidPath := filepath.Join(dir, pidFileName)
	_ = os.Remove(pidPath)
	return &ServerLock{lock: guard, pidPath: pidPath}, nil
}

// Publish records the process only after its API listener is ready.
func (l *ServerLock) Publish(pid int) error {
	if l == nil || l.lock == nil {
		return errors.New("server control lock is not held")
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.pidPath), ".opensbx.pid-*")
	if err != nil {
		return fmt.Errorf("create server process record: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := fmt.Fprintf(tmp, "%d\n", pid); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, l.pidPath); err != nil {
		return fmt.Errorf("publish server process record: %w", err)
	}
	return nil
}

func (l *ServerLock) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	if pid, err := readPID(l.pidPath); err == nil && pid == os.Getpid() {
		_ = os.Remove(l.pidPath)
	}
	return l.lock.Unlock()
}

// Running reports whether a server currently holds the control lock. Stale
// process records are removed after acquiring an otherwise-free lock.
func Running(dataDir string) (pid int, running bool, err error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return 0, false, err
	}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.IsDir() {
		return 0, false, fmt.Errorf("server control path %q is not a directory", dir)
	}
	guard := flock.New(filepath.Join(dir, lockFileName))
	acquired, err := guard.TryLock()
	if err != nil {
		return 0, false, fmt.Errorf("check server process: %w", err)
	}
	if acquired {
		_ = os.Remove(filepath.Join(dir, pidFileName))
		_ = guard.Unlock()
		return 0, false, nil
	}
	pid, _ = readPID(filepath.Join(dir, pidFileName))
	return pid, true, nil
}

// Stop sends the platform's graceful termination signal and waits for release.
func Stop(dataDir string, wait time.Duration) (int, error) {
	pid, running, err := Running(dataDir)
	if err != nil {
		return 0, err
	}
	if !running {
		return 0, ErrNotRunning
	}
	if pid <= 0 {
		return 0, errors.New("OpenSBX is starting; wait a moment and retry `opensbx stop`")
	}
	if err := signalProcess(pid); err != nil {
		if _, active, checkErr := Running(dataDir); checkErr == nil && !active {
			return pid, nil
		}
		return 0, fmt.Errorf("send shutdown signal to OpenSBX (PID %d): %w", pid, err)
	}
	if wait <= 0 {
		wait = 15 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		_, active, err := Running(dataDir)
		if err != nil {
			return 0, err
		}
		if !active {
			return pid, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("OpenSBX (PID %d) did not stop within %s; check its log and retry", pid, wait)
}

func readPID(path string) (int, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid OpenSBX process record %q", path)
	}
	return pid, nil
}

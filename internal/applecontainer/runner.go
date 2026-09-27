// Package applecontainer implements the sandbox contract using Apple container
// 1.4.1's CLI JSON schema. No host shell or runtime compiler is used.
package applecontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const outputLimit = 4 << 20

// Runner is the command boundary for hermetic backend tests. Run must finish
// before returning; Start must not attach the HTTP request's cancellation.
type Runner interface {
	Run(context.Context, []string, io.Reader, io.Writer, io.Writer) error
	Start([]string, io.Reader, io.Writer, io.Writer) (Process, error)
}

type Process interface {
	Wait() error
	Kill() error
}

type cliRunner struct{ path string }
type cliProcess struct{ cmd *exec.Cmd }

func (p cliProcess) Wait() error { return p.cmd.Wait() }
func (p cliProcess) Kill() error { return p.cmd.Process.Kill() }

func cliEnv() []string {
	// Retain the native user's launchd/keychain context, not API-supplied env.
	// In particular, do not forward CONTAINER_*, DYLD_*, or arbitrary secrets.
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "TMPDIR", "XPC_SERVICE_NAME"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (r cliRunner) Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = cliEnv(), in, out, stderr
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func (r cliRunner) Start(args []string, in io.Reader, out, stderr io.Writer) (Process, error) {
	cmd := exec.Command(r.path, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = cliEnv(), in, out, stderr
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cliProcess{cmd}, nil
}

// boundedBuffer drains all writes while retaining at most limit bytes. Protocol
// output is rejected on overflow rather than parsed after silent truncation.
type boundedBuffer struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	overflow bool
	tail     bool
	total    uint64
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += uint64(n)
	if len(b.data)+n > b.limit {
		b.overflow = true
		if b.tail {
			if n >= b.limit {
				b.data = append(b.data[:0], p[n-b.limit:]...)
				return n, nil
			}
			copy(b.data, b.data[len(b.data)+n-b.limit:])
			b.data = b.data[:b.limit-n]
		}
	}
	remaining := b.limit - len(b.data)
	if n > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *boundedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data), b.overflow
}

func (c *Client) run(ctx context.Context, in io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out := &boundedBuffer{limit: outputLimit}
	diag := &boundedBuffer{limit: 8192, tail: true}
	err := c.runner.Run(ctx, args, in, out, diag)
	s, overflow := out.snapshot()
	if err != nil {
		// Arguments and native diagnostics can contain credentials or host paths.
		// Do not reflect them through the unchanged public error handlers.
		diagnostic, truncated := diag.snapshot()
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && offlineImageUnavailable(args, diagnostic, truncated) {
			return nil, errOfflineImageUnavailable
		}
		return nil, fmt.Errorf("Apple container %s failed: %w", args[0], safeError(err))
	}
	if overflow {
		return nil, fmt.Errorf("Apple container output exceeds %d bytes", outputLimit)
	}
	return []byte(s), nil
}
func safeError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return fmt.Errorf("CLI or guest exited with status %d", exit.ExitCode())
	}
	return errors.New("CLI execution failed")
}

// Resolve validates the host and resolves PATH exactly once. It never installs
// software or starts system services. Health/version validation is done by Ping.
func Resolve(ctx context.Context) (Runner, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, errors.New("Apple container requires Apple Silicon macOS 26 or later; choose `-runtime docker` on this host. Setup: https://github.com/apple/container")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read macOS version: %w", err)
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(out.String()), ".")[0])
	if err != nil || major < 26 {
		return nil, errors.New("Apple Container requires macOS 26 or later; upgrade macOS or choose `-runtime docker`. Setup: https://github.com/apple/container")
	}
	path, err := exec.LookPath("container")
	if err != nil {
		return nil, errors.New("Apple container CLI not found in PATH (`container`). Install the signed package from https://github.com/apple/container/releases, then run `container system start` and retry `opensbx start -runtime container`")
	}
	return cliRunner{path}, nil
}

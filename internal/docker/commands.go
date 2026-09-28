package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"

	"github.com/moby/moby/api/pkg/stdcopy"
	moby "github.com/moby/moby/client"
)

// runningCommand tracks a command that is currently executing.
type runningCommand struct {
	execID    string             // Docker exec instance ID
	sandboxID string             // parent sandbox container ID
	cmd       []string           // original command (for pkill pattern)
	cancel    context.CancelFunc // cancels the exec context
	stdout    *ringBuffer        // captures stdout
	stderr    *ringBuffer        // captures stderr
	done      chan struct{}      // closed when command finishes
	attached  chan struct{}      // closed when the original attach attempt returns
	mu        sync.Mutex
	startErr  error
	exitCode  int
	finished  bool
}

func (c *Client) clearCommands(id string) {
	c.commands.Range(func(key, value any) bool {
		rc := value.(*runningCommand)
		if rc.sandboxID == id {
			rc.cancel()
			c.commands.Delete(key)
		}
		return true
	})
}

// generateCmdID creates a command ID: cmd_ + 40 hex chars.
func generateCmdID() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "cmd_" + hex.EncodeToString(b)
}

// ExecCommand creates and starts a command asynchronously inside a sandbox.
// Returns the CommandDetail immediately (no exit_code yet).
func (c *Client) ExecCommand(ctx context.Context, sandboxID string, req runtimeio.ExecCommandRequest) (runtimeio.CommandDetail, error) {
	if err := c.lockLifecycle(ctx); err != nil {
		return runtimeio.CommandDetail{}, err
	}
	defer c.lifecycleMu.Unlock()
	if err := c.repo.RequireIdle("docker", sandboxID); err != nil {
		return runtimeio.CommandDetail{}, err
	}
	// Verify sandbox is running.
	info, err := c.cli.ContainerInspect(ctx, sandboxID, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.CommandDetail{}, wrapNotFound(err)
	}
	if !info.Container.State.Running {
		return runtimeio.CommandDetail{}, sandbox.ErrNotRunning
	}

	cmdID := generateCmdID()
	now := time.Now().UnixMilli()

	// Build full command.
	fullCmd := append([]string{req.Command}, req.Args...)

	// Build env slice.
	var envSlice []string
	for k, v := range req.Env {
		envSlice = append(envSlice, k+"="+v)
	}

	// Create Docker exec instance.
	execOpts := moby.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          fullCmd,
		Env:          envSlice,
	}
	if req.Cwd != "" {
		execOpts.WorkingDir = req.Cwd
	}

	execCfg, err := c.cli.ExecCreate(ctx, sandboxID, execOpts)
	if err != nil {
		return runtimeio.CommandDetail{}, wrapNotFound(err)
	}

	// Persist command to DB.
	argsJSON, _ := json.Marshal(req.Args)
	if err := c.repo.SaveCommand(database.Command{
		ID:        cmdID,
		SandboxID: sandboxID,
		Name:      req.Command,
		Args:      string(argsJSON),
		Cwd:       req.Cwd,
		StartedAt: now,
	}); err != nil {
		return runtimeio.CommandDetail{}, fmt.Errorf("save command: %w", err)
	}

	// Set up ring buffers and tracking.
	stdoutBuf := newRingBuffer(defaultRingSize)
	stderrBuf := newRingBuffer(defaultRingSize)
	execCtx, cancel := context.WithCancel(context.Background())

	rc := &runningCommand{
		execID:    execCfg.ID,
		sandboxID: sandboxID,
		cmd:       fullCmd,
		cancel:    cancel,
		stdout:    stdoutBuf,
		stderr:    stderrBuf,
		done:      make(chan struct{}),
		attached:  make(chan struct{}),
	}
	c.commands.Store(cmdID, rc)

	// Launch goroutine to attach and stream output.
	go func() {
		defer func() {
			stdoutBuf.Close()
			stderrBuf.Close()
			close(rc.done)

			// Schedule cleanup from map after 5 minutes.
			time.AfterFunc(5*time.Minute, func() {
				c.commands.Delete(cmdID)
			})
		}()

		attached, err := c.cli.ExecAttach(execCtx, execCfg.ID, moby.ExecAttachOptions{})
		if err != nil {
			log.Printf("exec attach %s: %v", cmdID, err)
			rc.mu.Lock()
			rc.startErr = err
			rc.exitCode = -1
			rc.finished = true
			rc.mu.Unlock()
			close(rc.attached)
			c.repo.UpdateCommandFinished(cmdID, -1, time.Now().UnixMilli())
			return
		}
		defer attached.Close()
		close(rc.attached)

		// Demux stdout/stderr into ring buffers.
		stdcopy.StdCopy(stdoutBuf, stderrBuf, attached.Reader)

		// Get exit code.
		exitCode := -1
		inspect, err := c.cli.ExecInspect(context.Background(), execCfg.ID, moby.ExecInspectOptions{})
		if err == nil {
			exitCode = inspect.ExitCode
		}

		finishedAt := time.Now().UnixMilli()
		rc.mu.Lock()
		rc.exitCode = exitCode
		rc.finished = true
		rc.mu.Unlock()

		c.repo.UpdateCommandFinished(cmdID, exitCode, finishedAt)
	}()

	return runtimeio.CommandDetail{
		ID:        cmdID,
		Name:      req.Command,
		Args:      req.Args,
		Cwd:       req.Cwd,
		SandboxID: sandboxID,
		StartedAt: now,
	}, nil
}

// GetCommand returns command details by ID.
func (c *Client) GetCommand(ctx context.Context, sandboxID, cmdID string) (runtimeio.CommandDetail, error) {
	dbCmd, err := c.repo.FindCommandByID(cmdID)
	if err != nil {
		return runtimeio.CommandDetail{}, err
	}
	if dbCmd == nil {
		return runtimeio.CommandDetail{}, sandbox.ErrCommandNotFound
	}
	if dbCmd.SandboxID != sandboxID {
		return runtimeio.CommandDetail{}, sandbox.ErrCommandNotFound
	}

	return c.dbCommandToDetail(*dbCmd), nil
}

// ListCommands returns all commands for a sandbox.
func (c *Client) ListCommands(ctx context.Context, sandboxID string) ([]runtimeio.CommandDetail, error) {
	// Verify sandbox exists.
	if _, err := c.cli.ContainerInspect(ctx, sandboxID, moby.ContainerInspectOptions{}); err != nil {
		return nil, wrapNotFound(err)
	}

	dbCmds, err := c.repo.FindCommandsBySandbox(sandboxID)
	if err != nil {
		return nil, err
	}

	details := make([]runtimeio.CommandDetail, 0, len(dbCmds))
	for _, cmd := range dbCmds {
		details = append(details, c.dbCommandToDetail(cmd))
	}
	return details, nil
}

// KillCommand sends a signal to a running command.
func (c *Client) KillCommand(ctx context.Context, sandboxID, cmdID string, signal int) (runtimeio.CommandDetail, error) {
	if err := ctx.Err(); err != nil {
		return runtimeio.CommandDetail{}, err
	}
	if signal < 1 || signal > 64 {
		return runtimeio.CommandDetail{}, fmt.Errorf("%w: Linux signal must be between 1 and 64", sandbox.ErrInvalidInput)
	}
	// Look up running command.
	v, ok := c.commands.Load(cmdID)
	if !ok {
		// Check if it exists in DB.
		dbCmd, err := c.repo.FindCommandByID(cmdID)
		if err != nil {
			return runtimeio.CommandDetail{}, err
		}
		if dbCmd == nil {
			return runtimeio.CommandDetail{}, sandbox.ErrCommandNotFound
		}
		return runtimeio.CommandDetail{}, sandbox.ErrCommandFinished
	}

	rc := v.(*runningCommand)
	rc.mu.Lock()
	if rc.sandboxID != sandboxID {
		rc.mu.Unlock()
		return runtimeio.CommandDetail{}, sandbox.ErrCommandNotFound
	}
	cmd := rc.cmd
	rc.mu.Unlock()
	if err := c.waitCommandRunning(ctx, rc); err != nil {
		return runtimeio.CommandDetail{}, err
	}

	// pkill uses an extended regex over space-joined argv, not shell syntax.
	// Quote the literal command and anchor the whole match; pass it as one argument
	// without a shell so quotes, substitutions and newlines remain ordinary data.
	pattern := "^" + regexp.QuoteMeta(strings.Join(cmd, " ")) + "$"
	result, err := c.execWithStdin(ctx, sandboxID, []string{"pkill", "-" + strconv.Itoa(signal), "-f", "--", pattern}, nil)
	if err != nil {
		return runtimeio.CommandDetail{}, err
	}
	if err := ctx.Err(); err != nil {
		return runtimeio.CommandDetail{}, err
	}
	if result.exitCode != 0 {
		rc.mu.Lock()
		finished := rc.finished
		rc.mu.Unlock()
		if finished {
			return runtimeio.CommandDetail{}, sandbox.ErrCommandFinished
		}
		return runtimeio.CommandDetail{}, fmt.Errorf("command signal failed: pkill exited with code %d", result.exitCode)
	}

	// Wait briefly for the command to finish, then return current state.
	select {
	case <-rc.done:
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return runtimeio.CommandDetail{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return runtimeio.CommandDetail{}, err
	}

	return c.GetCommand(ctx, sandboxID, cmdID)
}

// Both the HTTP upgrade and Running=true can precede native process creation.
// Docker publishes a positive PID only after its exec process Start succeeds.
// Use it solely as a start acknowledgment, never as a guest-namespace kill PID.
// A canceled kill never cancels the payload.
func (c *Client) waitCommandRunning(ctx context.Context, rc *runningCommand) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	terminal := func() error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.startErr != nil {
			return rc.startErr
		}
		if rc.finished {
			return sandbox.ErrCommandFinished
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := terminal(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-rc.done:
		if err := terminal(); err != nil {
			return err
		}
		return sandbox.ErrCommandFinished
	case <-rc.attached:
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := terminal(); err != nil {
			return err
		}
		inspect, err := c.cli.ExecInspect(ctx, rc.execID, moby.ExecInspectOptions{})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := terminal(); err != nil {
			return err
		}
		if inspect.Running && inspect.PID > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rc.done:
			if err := terminal(); err != nil {
				return err
			}
			return sandbox.ErrCommandFinished
		case <-ticker.C:
		}
	}
}

// StreamCommandLogs returns readers for stdout and stderr of a command.
func (c *Client) StreamCommandLogs(ctx context.Context, sandboxID, cmdID string) (io.ReadCloser, io.ReadCloser, error) {
	v, ok := c.commands.Load(cmdID)
	if !ok {
		return nil, nil, sandbox.ErrCommandNotFound
	}

	rc := v.(*runningCommand)
	if rc.sandboxID != sandboxID {
		return nil, nil, sandbox.ErrCommandNotFound
	}

	return rc.stdout.NewReader(), rc.stderr.NewReader(), nil
}

// GetCommandLogs returns a snapshot of stdout and stderr for a command without streaming.
func (c *Client) GetCommandLogs(ctx context.Context, sandboxID, cmdID string) (runtimeio.CommandLogsResponse, error) {
	v, ok := c.commands.Load(cmdID)
	if !ok {
		return runtimeio.CommandLogsResponse{}, sandbox.ErrCommandNotFound
	}

	rc := v.(*runningCommand)
	if rc.sandboxID != sandboxID {
		return runtimeio.CommandLogsResponse{}, sandbox.ErrCommandNotFound
	}

	rc.mu.Lock()
	exitCode := (*int)(nil)
	if rc.finished {
		ec := rc.exitCode
		exitCode = &ec
	}
	rc.mu.Unlock()

	return runtimeio.CommandLogsResponse{
		Stdout:   string(rc.stdout.Bytes()),
		Stderr:   string(rc.stderr.Bytes()),
		ExitCode: exitCode,
	}, nil
}

// WaitCommand blocks until a command finishes and returns the updated detail.
func (c *Client) WaitCommand(ctx context.Context, sandboxID, cmdID string) (runtimeio.CommandDetail, error) {
	v, ok := c.commands.Load(cmdID)
	if !ok {
		// Already finished and cleaned up, or doesn't exist.
		return c.GetCommand(ctx, sandboxID, cmdID)
	}

	rc := v.(*runningCommand)
	select {
	case <-rc.done:
	case <-ctx.Done():
		return runtimeio.CommandDetail{}, ctx.Err()
	}

	return c.GetCommand(ctx, sandboxID, cmdID)
}

// dbCommandToDetail reconstructs an adapter-local command record.
func (c *Client) dbCommandToDetail(cmd database.Command) runtimeio.CommandDetail {
	var args []string
	if cmd.Args != "" {
		json.Unmarshal([]byte(cmd.Args), &args)
	}

	detail := runtimeio.CommandDetail{
		ID:         cmd.ID,
		Name:       cmd.Name,
		Args:       args,
		Cwd:        cmd.Cwd,
		SandboxID:  cmd.SandboxID,
		ExitCode:   cmd.ExitCode,
		StartedAt:  cmd.StartedAt,
		FinishedAt: cmd.FinishedAt,
	}

	// If the command is still running in memory, check live state.
	if v, ok := c.commands.Load(cmd.ID); ok {
		rc := v.(*runningCommand)
		rc.mu.Lock()
		if rc.finished {
			ec := rc.exitCode
			detail.ExitCode = &ec
		}
		rc.mu.Unlock()
	}

	return detail
}

// execResult holds the output from a synchronous exec (used internally for file operations).
type execResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// execWithStdin runs a command with optional stdin, returning separated stdout/stderr and exit code.
func (c *Client) execWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (execResult, error) {
	attachStdin := stdin != nil
	execCfg, err := c.cli.ExecCreate(ctx, id, moby.ExecCreateOptions{
		AttachStdin:  attachStdin,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return execResult{}, wrapNotFound(err)
	}

	attached, err := c.cli.ExecAttach(ctx, execCfg.ID, moby.ExecAttachOptions{})
	if err != nil {
		return execResult{}, err
	}
	defer attached.Close()
	// Hijacked streams outlive the HTTP request. Close explicitly on cancellation
	// so a stalled signal exec cannot hide the caller's canceled/deadline error.
	stopClose := context.AfterFunc(ctx, attached.Close)
	defer stopClose()

	if stdin != nil {
		if _, err := io.Copy(attached.Conn, stdin); err != nil {
			if ctx.Err() != nil {
				return execResult{}, ctx.Err()
			}
			return execResult{}, err
		}
		attached.CloseWrite()
	}

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil && err != io.EOF {
		if ctx.Err() != nil {
			return execResult{}, ctx.Err()
		}
		return execResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return execResult{}, err
	}

	// Inspect the exec instance to retrieve the exit code.
	inspect, err := c.cli.ExecInspect(ctx, execCfg.ID, moby.ExecInspectOptions{})
	if err != nil {
		return execResult{}, err
	}

	return execResult{
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		exitCode: inspect.ExitCode,
	}, nil
}

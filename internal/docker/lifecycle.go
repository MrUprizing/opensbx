package docker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

// timerEntry holds a timer and a cancel channel to avoid goroutine leaks.
type timerEntry struct {
	timer     *time.Timer
	cancel    chan struct{}
	expiresAt time.Time
}

// defaultTimeout is applied when no timeout is specified (15 minutes).
const defaultTimeout = 900

// Default resource limits (1 vCPU, 1GB RAM)
const (
	defaultMemoryMB = 1024 // 1GB
	defaultCPUs     = 1.0  // 1 vCPU
)

// Maximum resource limits (4 vCPU, 8GB RAM)
const (
	maxMemoryMB = 8192 // 8GB
	maxCPUs     = 4.0  // 4 vCPU
)

// Create creates and starts a sandbox. Docker assigns host ports automatically.
// Applies optional resource limits and schedules auto-stop with a default TTL of 15 minutes.
// Returns ErrImageNotFound if the image does not exist locally.
func (c *Client) Create(ctx context.Context, req runtimeio.CreateSandboxRequest) (response runtimeio.CreateSandboxResponse, createErr error) {
	if err := c.lockLifecycle(ctx); err != nil {
		return response, err
	}
	defer c.lifecycleMu.Unlock()
	// Verify image exists locally
	exists, err := c.ImageExists(ctx, req.Image)
	if err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}
	if !exists {
		return runtimeio.CreateSandboxResponse{}, sandbox.ErrImageNotFound
	}

	ports := normalizePorts(req.Ports)
	mainPort := ""
	if len(ports) > 0 {
		mainPort = ports[0]
	}

	cfg := &container.Config{
		Image:        req.Image,
		Env:          req.Env,
		Cmd:          []string{"sleep", "infinity"},
		ExposedPorts: buildExposedPorts(ports),
	}

	hostCfg := &container.HostConfig{
		PortBindings: buildPortBindings(ports),
	}

	// Apply resource limits (defaults: 1GB RAM, 1 vCPU)
	memory := int64(defaultMemoryMB)
	cpus := defaultCPUs
	if req.Resources != nil {
		if req.Resources.Memory > 0 {
			memory = req.Resources.Memory
		}
		if req.Resources.CPUs > 0 {
			cpus = req.Resources.CPUs
		}
	}
	hostCfg.Resources = container.Resources{
		Memory:   memory * 1024 * 1024, // MB to bytes
		NanoCPUs: int64(cpus * 1e9),
	}

	// Auto-generate a unique sandbox name.
	name := generateUniqueName(func(n string) bool {
		sb, _ := c.repo.FindByName(n)
		return sb != nil
	})
	token := generateCmdID()
	// A prechosen exact name and unpredictable label survive a lost create response.
	nativeName := name + "-" + token
	cfg.Labels = map[string]string{ownerLabel: token}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	op := database.Operation{ID: "docker:" + token, RuntimeKind: "docker", PublicID: sandbox.CreationID(ctx, ""), NativeName: nativeName, Token: token, Kind: "create", Deadline: &deadline}
	if err := c.repo.BeginCreate(op); err != nil {
		return response, err
	}
	committed := false
	defer func() {
		if !committed {
			c.retryOperation(op)
		}
	}()

	result, err := c.cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: hostCfg,
		Name:       nativeName,
	})
	if err != nil {
		return runtimeio.CreateSandboxResponse{}, fmt.Errorf("create intent %s retained for recovery: %w", op.ID, err)
	}
	defer func() {
		if committed {
			return
		}
		c.cancelTimer(result.ID)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.cli.ContainerRemove(cleanupCtx, result.ID, moby.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			// Retain ownership if a newly created resource could not be rolled back.
			saveErr := c.repo.CreateOwnership(database.Sandbox{ID: sandbox.CreationID(ctx, result.ID), NativeID: result.ID, Name: name, Image: sandbox.CreationImage(ctx, req.Image), Port: mainPort, RuntimeKind: "docker", AttemptToken: token, ExpiresAt: &deadline})
			createErr = errors.Join(createErr, fmt.Errorf("rollback sandbox %s: %w", result.ID, err), saveErr)
		} else {
			createErr = errors.Join(createErr, c.repo.DeleteOperation(op))
		}
	}()
	if err := c.repo.BindCreation(&op, result.ID); err != nil {
		return response, err
	}

	if _, err := c.cli.ContainerStart(ctx, result.ID, moby.ContainerStartOptions{}); err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}

	// Inspect to get Docker-assigned host ports.
	info, err := c.cli.ContainerInspect(ctx, result.ID, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}

	assignedPorts := extractPorts(info.Container.NetworkSettings.Ports)

	// A successful create must have durable ownership metadata.
	if err := c.repo.CommitCreation(op, database.Sandbox{
		ID:          sandbox.CreationID(ctx, result.ID),
		NativeID:    result.ID,
		Name:        name,
		Image:       sandbox.CreationImage(ctx, req.Image),
		Ports:       database.JSONMap(assignedPorts),
		Port:        mainPort,
		RuntimeKind: "docker", AttemptToken: token, ExpiresAt: &deadline,
	}, runtimeio.AwaitAdoption(ctx)); err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}
	if runtimeio.AwaitAdoption(ctx) {
		c.createAttempts.Store(result.ID, op)
	}
	committed = true
	c.scheduleDeadline(result.ID, deadline, time.Until(deadline))

	return runtimeio.CreateSandboxResponse{
		ID:    result.ID,
		Name:  name,
		Ports: portKeys(assignedPorts),
	}, nil
}

func (c *Client) DiscardCreated(ctx context.Context, id string) error {
	err := c.Remove(ctx, id)
	if err != nil {
		c.CreationFailed(id)
	} else {
		c.createAttempts.Delete(id)
	}
	return err
}

// Start starts a stopped sandbox and re-schedules the auto-stop timer.
// Returns ErrAlreadyRunning (409) if the sandbox is already running.
func (c *Client) Start(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	if err := c.lockLifecycle(ctx); err != nil {
		return runtimeio.RestartResponse{}, err
	}
	defer c.lifecycleMu.Unlock()
	// Check current state to return a meaningful conflict error.
	pre, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}
	if pre.Container.State.Running {
		return runtimeio.RestartResponse{}, sandbox.ErrAlreadyRunning
	}

	deadline := time.Now().Add(defaultTimeout * time.Second)
	op, err := c.repo.BeginOperation("docker", id, "start", &deadline)
	if err != nil {
		return runtimeio.RestartResponse{}, err
	}
	defer c.retryOperation(*op)
	if err := c.checkOwnership(ctx, *op); err != nil {
		return runtimeio.RestartResponse{}, err
	}
	defer c.invalidateCache(id)
	if _, err := c.cli.ContainerStart(ctx, id, moby.ContainerStartOptions{}); err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}

	c.scheduleDeadline(id, deadline, time.Until(deadline))

	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}

	var expiresAt *time.Time
	if entry := c.getTimerEntry(id); entry != nil {
		ea := entry.expiresAt
		expiresAt = &ea
	}

	ports := extractPorts(info.Container.NetworkSettings.Ports)

	if dbErr := c.repo.CompleteOperation(*op, database.JSONMap(ports), &deadline); dbErr != nil {
		return runtimeio.RestartResponse{}, fmt.Errorf("persist ports for sandbox %s: %w", id, dbErr)
	}

	return runtimeio.RestartResponse{
		Status:    "started",
		Ports:     portKeys(ports),
		ExpiresAt: expiresAt,
	}, nil
}

// Stop stops a running sandbox and cancels its expiration timer.
// Returns ErrAlreadyStopped (409) if the sandbox is not running.
func (c *Client) Stop(ctx context.Context, id string) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	return c.stopLocked(ctx, id)
}

func (c *Client) stopLocked(ctx context.Context, id string) error {
	op, err := c.repo.BeginOperation("docker", id, "stop", nil)
	if err != nil {
		return err
	}
	defer c.retryOperation(*op)
	if err := c.checkOwnership(ctx, *op); err != nil {
		return err
	}
	defer c.invalidateCache(id)
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			if err := c.repo.CompleteOperation(*op, nil, nil); err != nil {
				return err
			}
			c.cancelTimer(id)
			return sandbox.ErrNotFound
		}
		return runtimeio.DeferRecovery(wrapNotFound(err))
	}
	if !info.Container.State.Running {
		if err := c.repo.CompleteOperation(*op, nil, nil); err != nil {
			return err
		}
		c.cancelTimer(id)
		return sandbox.ErrAlreadyStopped
	}

	_, err = c.cli.ContainerStop(ctx, id, moby.ContainerStopOptions{})
	if err == nil || errdefs.IsNotFound(err) {
		if err := c.repo.CompleteOperation(*op, nil, nil); err != nil {
			return err
		}
		c.cancelTimer(id)
	}
	return runtimeio.DeferRecovery(wrapNotFound(err))
}

// Restart restarts a sandbox and returns the new port mappings.
// It cancels any existing timer and schedules a fresh one with the default timeout.
func (c *Client) Restart(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	if err := c.lockLifecycle(ctx); err != nil {
		return runtimeio.RestartResponse{}, err
	}
	defer c.lifecycleMu.Unlock()
	deadline := time.Now().Add(defaultTimeout * time.Second)
	op, err := c.repo.BeginOperation("docker", id, "restart", &deadline)
	if err != nil {
		return runtimeio.RestartResponse{}, err
	}
	defer c.retryOperation(*op)
	if err := c.checkOwnership(ctx, *op); err != nil {
		return runtimeio.RestartResponse{}, err
	}
	defer c.invalidateCache(id)

	if _, err := c.cli.ContainerRestart(ctx, id, moby.ContainerRestartOptions{}); err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}

	// Re-schedule auto-stop with the default timeout.
	c.scheduleDeadline(id, deadline, time.Until(deadline))

	// Inspect to get the new ports.
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}

	var expiresAt *time.Time
	if entry := c.getTimerEntry(id); entry != nil {
		ea := entry.expiresAt
		expiresAt = &ea
	}

	ports := extractPorts(info.Container.NetworkSettings.Ports)

	// Update persisted ports after restart (they may change).
	if dbErr := c.repo.CompleteOperation(*op, database.JSONMap(ports), &deadline); dbErr != nil {
		return runtimeio.RestartResponse{}, fmt.Errorf("persist ports for sandbox %s: %w", id, dbErr)
	}

	return runtimeio.RestartResponse{
		Status:    "restarted",
		Ports:     portKeys(ports),
		ExpiresAt: expiresAt,
	}, nil
}

// Remove removes a sandbox forcefully and cancels its expiration timer.
// If the container no longer exists in Docker, it still cleans up the DB record.
func (c *Client) Remove(ctx context.Context, id string) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	op, err := c.repo.BeginOperation("docker", id, "delete", nil)
	if err != nil {
		return err
	}
	defer c.retryOperation(*op)
	if err := c.checkOwnership(ctx, *op); err != nil {
		return err
	}
	_, err = c.cli.ContainerRemove(ctx, id, moby.ContainerRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	c.cancelTimer(id)
	c.invalidateCache(id)

	// Kill all running commands for this sandbox.
	c.clearCommands(id)

	if dbErr := c.repo.DeleteOperation(*op); dbErr != nil {
		return fmt.Errorf("delete sandbox %s: %w", id, dbErr)
	}
	return nil
}

// Pause pauses a running sandbox (freezes all processes).
// Returns ErrNotRunning (409) if the sandbox is not running,
// or ErrAlreadyPaused (409) if it is already paused.
func (c *Client) Pause(ctx context.Context, id string) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	if err := c.repo.RequireIdle("docker", id); err != nil {
		return err
	}
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return wrapNotFound(err)
	}
	if info.Container.State.Paused {
		return sandbox.ErrAlreadyPaused
	}
	if !info.Container.State.Running {
		return sandbox.ErrNotRunning
	}

	_, err = c.cli.ContainerPause(ctx, id, moby.ContainerPauseOptions{})
	return wrapNotFound(err)
}

// Resume unpauses a paused sandbox.
// Returns ErrNotPaused (409) if the sandbox is not currently paused.
func (c *Client) Resume(ctx context.Context, id string) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	if err := c.repo.RequireIdle("docker", id); err != nil {
		return err
	}
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return wrapNotFound(err)
	}
	if !info.Container.State.Paused {
		return sandbox.ErrNotPaused
	}

	_, err = c.cli.ContainerUnpause(ctx, id, moby.ContainerUnpauseOptions{})
	return wrapNotFound(err)
}

// RenewExpiration resets the auto-stop timer for a sandbox.
func (c *Client) RenewExpiration(ctx context.Context, id string, timeout int) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	// Verify the sandbox exists.
	if _, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{}); err != nil {
		return wrapNotFound(err)
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	if err := c.repo.SetDeadline(id, "docker", &deadline); err != nil {
		return err
	}
	c.scheduleDeadline(id, deadline, time.Until(deadline))
	return nil
}

// Shutdown cancels all pending timers, running commands, and stops tracked containers.
// Called during graceful shutdown to prevent orphaned containers.
func (c *Client) Shutdown(ctx context.Context) {
	c.recovery.Stop()
	if err := c.lockLifecycle(ctx); err != nil {
		log.Printf("docker shutdown: %v", err)
		return
	}
	defer c.lifecycleMu.Unlock()
	c.closing = true
	commandCount := 0
	c.commands.Range(func(_, _ any) bool {
		commandCount++
		return true
	})

	timerCount := 0
	c.timers.Range(func(_, _ any) bool {
		timerCount++
		return true
	})

	log.Printf("docker shutdown: canceling %d commands, stopping %d sandboxes", commandCount, timerCount)

	// Cancel all running commands.
	c.commands.Range(func(key, value any) bool {
		rc := value.(*runningCommand)
		rc.cancel()
		return true
	})

	c.timers.Range(func(key, value any) bool {
		id := key.(string)
		c.cancelTimer(id)
		if err := c.stopForShutdown(ctx, id); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("docker shutdown: stop sandbox %s timeout", id)
			} else {
				log.Printf("docker shutdown: stop sandbox %s: %v", id, err)
			}
		}
		return true
	})
}

// scheduleStop creates a timer that auto-stops the sandbox after the given seconds.
// Uses a cancel channel so cancelTimer can cleanly terminate the goroutine.
func (c *Client) scheduleStop(id string, seconds int) {
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	c.scheduleDeadline(id, deadline, time.Until(deadline))
}

func (c *Client) scheduleDeadline(id string, deadline time.Time, delay time.Duration) {
	c.timersMu.Lock()
	defer c.timersMu.Unlock()
	c.cancelTimerLocked(id)
	timer := time.NewTimer(delay)
	cancel := make(chan struct{})

	entry := &timerEntry{
		timer:     timer,
		cancel:    cancel,
		expiresAt: deadline,
	}
	c.timers.Store(id, entry)

	go func() {
		select {
		case <-timer.C:
			c.expire(id, entry)
		case <-cancel:
			// Timer was cancelled; stop it and drain the channel if needed.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}()
}

// cancelTimer stops and removes the expiration timer for a sandbox.
func (c *Client) cancelTimer(id string) {
	c.timersMu.Lock()
	defer c.timersMu.Unlock()
	c.cancelTimerLocked(id)
}

func (c *Client) cancelTimerLocked(id string) {
	if v, ok := c.timers.LoadAndDelete(id); ok {
		entry := v.(*timerEntry)
		entry.timer.Stop()
		close(entry.cancel)
	}
}

// Lifecycle operations share a lock with expiration so an old callback cannot
// stop a newly started or renewed sandbox. Waiting respects caller cancellation.
func (c *Client) lockLifecycle(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.lifecycleMu.TryLock() {
			if c.closing {
				c.lifecycleMu.Unlock()
				return errors.New("Docker backend is shutting down")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *Client) expire(id string, entry *timerEntry) {
	key := fmt.Sprintf("ttl:%s:%p", id, entry)
	c.recovery.Schedule(key, func(ctx context.Context) error {
		if err := c.lockLifecycle(ctx); err != nil {
			return err
		}
		defer c.lifecycleMu.Unlock()
		c.expireLocked(ctx, id, entry)
		return nil
	})
	_ = c.recovery.Run(context.Background(), key)
}

func (c *Client) expireLocked(ctx context.Context, id string, entry *timerEntry) {
	if c.closing || c.getTimerEntry(id) != entry {
		return
	}
	if err := c.reconcileResource(ctx, id); err != nil {
		log.Printf("docker sandbox TTL recovery failed for %s: %v", id, err)
		c.scheduleDeadline(id, entry.expiresAt, 30*time.Second)
		return
	}
	if c.getTimerEntry(id) == nil {
		return
	} // Reconciliation already stopped or removed it.
	row, err := c.repo.FindByNativeID(id)
	if err != nil {
		log.Printf("Docker TTL ownership lookup %s: %v", id, err)
		c.scheduleDeadline(id, entry.expiresAt, 30*time.Second)
		return
	}
	if row == nil {
		c.cancelTimer(id)
		return
	}
	if row.ExpiresAt != nil && row.ExpiresAt.After(entry.expiresAt) && row.ExpiresAt.After(time.Now()) {
		c.scheduleDeadline(id, *row.ExpiresAt, time.Until(*row.ExpiresAt))
		return
	}
	if err := c.stopLocked(ctx, id); err != nil && !errors.Is(err, sandbox.ErrAlreadyStopped) && !errors.Is(err, sandbox.ErrNotFound) {
		log.Printf("docker sandbox TTL stop failed for %s: %v", id, err)
		c.scheduleDeadline(id, entry.expiresAt, 30*time.Second)
		return
	}
	c.cancelTimer(id)
	c.invalidateCache(id)
}

// getTimerEntry returns the timer entry for a sandbox, or nil if not tracked.
func (c *Client) getTimerEntry(id string) *timerEntry {
	if v, ok := c.timers.Load(id); ok {
		return v.(*timerEntry)
	}
	return nil
}

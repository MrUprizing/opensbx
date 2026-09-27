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

	result, err := c.cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: hostCfg,
		Name:       name,
	})
	if err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		c.cancelTimer(result.ID)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.cli.ContainerRemove(cleanupCtx, result.ID, moby.ContainerRemoveOptions{Force: true}); err != nil {
			// Retain ownership if a newly created resource could not be rolled back.
			saveErr := c.repo.CreateOwnership(database.Sandbox{ID: sandbox.CreationID(ctx, result.ID), NativeID: result.ID, Name: name, Image: sandbox.CreationImage(ctx, req.Image), Port: mainPort})
			createErr = errors.Join(createErr, fmt.Errorf("rollback sandbox %s: %w", result.ID, err), saveErr)
		}
	}()

	if _, err := c.cli.ContainerStart(ctx, result.ID, moby.ContainerStartOptions{}); err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}

	// Schedule auto-stop. Default 15 min if not specified.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	c.scheduleStop(result.ID, timeout)

	// Inspect to get Docker-assigned host ports.
	info, err := c.cli.ContainerInspect(ctx, result.ID, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}

	assignedPorts := extractPorts(info.Container.NetworkSettings.Ports)

	// A successful create must have durable ownership metadata.
	if err := c.repo.CreateOwnership(database.Sandbox{
		ID:       sandbox.CreationID(ctx, result.ID),
		NativeID: result.ID,
		Name:     name,
		Image:    sandbox.CreationImage(ctx, req.Image),
		Ports:    database.JSONMap(assignedPorts),
		Port:     mainPort,
	}); err != nil {
		return runtimeio.CreateSandboxResponse{}, err
	}
	committed = true

	return runtimeio.CreateSandboxResponse{
		ID:    result.ID,
		Name:  name,
		Ports: portKeys(assignedPorts),
	}, nil
}

func (c *Client) DiscardCreated(ctx context.Context, id string) error { return c.Remove(ctx, id) }

// Start starts a stopped sandbox and re-schedules the auto-stop timer.
// Returns ErrAlreadyRunning (409) if the sandbox is already running.
func (c *Client) Start(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	// Check current state to return a meaningful conflict error.
	pre, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}
	if pre.Container.State.Running {
		return runtimeio.RestartResponse{}, sandbox.ErrAlreadyRunning
	}

	if _, err := c.cli.ContainerStart(ctx, id, moby.ContainerStartOptions{}); err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}

	c.scheduleStop(id, defaultTimeout)

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

	if dbErr := c.repo.UpdatePorts(id, database.JSONMap(ports)); dbErr != nil {
		log.Printf("database: failed to update ports for sandbox %s: %v", id, dbErr)
	}
	c.invalidateCache(id)

	return runtimeio.RestartResponse{
		Status:    "started",
		Ports:     portKeys(ports),
		ExpiresAt: expiresAt,
	}, nil
}

// Stop stops a running sandbox and cancels its expiration timer.
// Returns ErrAlreadyStopped (409) if the sandbox is not running.
func (c *Client) Stop(ctx context.Context, id string) error {
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return wrapNotFound(err)
	}
	if !info.Container.State.Running {
		return sandbox.ErrAlreadyStopped
	}

	c.cancelTimer(id)
	c.invalidateCache(id)
	_, err = c.cli.ContainerStop(ctx, id, moby.ContainerStopOptions{})
	return wrapNotFound(err)
}

// Restart restarts a sandbox and returns the new port mappings.
// It cancels any existing timer and schedules a fresh one with the default timeout.
func (c *Client) Restart(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	c.cancelTimer(id)

	if _, err := c.cli.ContainerRestart(ctx, id, moby.ContainerRestartOptions{}); err != nil {
		return runtimeio.RestartResponse{}, wrapNotFound(err)
	}

	// Re-schedule auto-stop with the default timeout.
	c.scheduleStop(id, defaultTimeout)

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
	if dbErr := c.repo.UpdatePorts(id, database.JSONMap(ports)); dbErr != nil {
		log.Printf("database: failed to update ports for sandbox %s: %v", id, dbErr)
	}
	c.invalidateCache(id)

	return runtimeio.RestartResponse{
		Status:    "restarted",
		Ports:     portKeys(ports),
		ExpiresAt: expiresAt,
	}, nil
}

// Remove removes a sandbox forcefully and cancels its expiration timer.
// If the container no longer exists in Docker, it still cleans up the DB record.
func (c *Client) Remove(ctx context.Context, id string) error {
	c.cancelTimer(id)
	c.invalidateCache(id)

	// Kill all running commands for this sandbox.
	c.commands.Range(func(key, value any) bool {
		rc := value.(*runningCommand)
		if rc.sandboxID == id {
			rc.cancel()
		}
		return true
	})

	_, err := c.cli.ContainerRemove(ctx, id, moby.ContainerRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}

	// Clean up command records from DB.
	if dbErr := c.repo.DeleteCommandsBySandbox(id); dbErr != nil {
		log.Printf("database: failed to delete commands for sandbox %s: %v", id, dbErr)
	}

	if dbErr := c.repo.Delete(id); dbErr != nil {
		log.Printf("database: failed to delete sandbox %s: %v", id, dbErr)
	}
	return nil
}

// Pause pauses a running sandbox (freezes all processes).
// Returns ErrNotRunning (409) if the sandbox is not running,
// or ErrAlreadyPaused (409) if it is already paused.
func (c *Client) Pause(ctx context.Context, id string) error {
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
	// Verify the sandbox exists.
	if _, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{}); err != nil {
		return wrapNotFound(err)
	}

	c.cancelTimer(id)
	c.scheduleStop(id, timeout)
	return nil
}

// Shutdown cancels all pending timers, running commands, and stops tracked containers.
// Called during graceful shutdown to prevent orphaned containers.
func (c *Client) Shutdown(ctx context.Context) {
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
		entry := value.(*timerEntry)
		entry.timer.Stop()
		close(entry.cancel)
		c.timers.Delete(id)
		if _, err := c.cli.ContainerStop(ctx, id, moby.ContainerStopOptions{}); err != nil {
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
	d := time.Duration(seconds) * time.Second
	timer := time.NewTimer(d)
	cancel := make(chan struct{})

	c.timers.Store(id, &timerEntry{
		timer:     timer,
		cancel:    cancel,
		expiresAt: time.Now().Add(d),
	})

	go func() {
		select {
		case <-timer.C:
			c.timers.Delete(id)
			c.cli.ContainerStop(context.Background(), id, moby.ContainerStopOptions{})
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
	if v, ok := c.timers.LoadAndDelete(id); ok {
		entry := v.(*timerEntry)
		close(entry.cancel)
	}
}

// getTimerEntry returns the timer entry for a sandbox, or nil if not tracked.
func (c *Client) getTimerEntry(id string) *timerEntry {
	if v, ok := c.timers.Load(id); ok {
		return v.(*timerEntry)
	}
	return nil
}

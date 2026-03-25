package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"opensbx/internal/database"
	"opensbx/models"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	moby "github.com/moby/moby/client"
)

// Client wraps the Docker SDK and exposes sandbox operations.
type Client struct {
	cli            *moby.Client
	repo           *database.Repository
	timers         sync.Map          // map[containerID]*timerEntry
	commands       sync.Map          // map[cmdID]*runningCommand
	onCacheInvalid func(name string) // called when a sandbox's ports change or it is removed
}

// runningCommand tracks a command that is currently executing.
type runningCommand struct {
	execID    string             // Docker exec instance ID
	sandboxID string             // parent sandbox container ID
	cmd       []string           // original command (for pkill pattern)
	cancel    context.CancelFunc // cancels the exec context
	stdout    *ringBuffer        // captures stdout
	stderr    *ringBuffer        // captures stderr
	done      chan struct{}      // closed when command finishes
	mu        sync.Mutex
	exitCode  int
	finished  bool
}

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

var (
	once       sync.Once
	mobyClient *moby.Client
)

// New creates a Docker Client with the given repository.
// The underlying Docker connection is a singleton (created once),
// but each Client gets its own repository.
func New(repo *database.Repository) *Client {
	once.Do(func() {
		cli, err := moby.NewClientWithOpts(moby.FromEnv, moby.WithAPIVersionNegotiation())
		if err != nil {
			panic(err)
		}
		mobyClient = cli
	})
	return &Client{cli: mobyClient, repo: repo}
}

// SetCacheInvalidator registers a callback invoked when a sandbox's ports
// change (restart) or it is stopped/removed, so the proxy cache stays fresh.
func (c *Client) SetCacheInvalidator(fn func(name string)) {
	c.onCacheInvalid = fn
}

// invalidateCache notifies the proxy that a sandbox's route may have changed.
func (c *Client) invalidateCache(containerID string) {
	if c.onCacheInvalid == nil {
		return
	}
	sb, err := c.repo.FindByID(containerID)
	if err == nil && sb != nil && sb.Name != "" {
		c.onCacheInvalid(sb.Name)
	}
}

// Ping checks connectivity with the Docker daemon.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx, moby.PingOptions{})
	return err
}

// List returns all sandboxes tracked in the database, enriched with live
// state from Docker. Stopped containers are always included.
func (c *Client) List(ctx context.Context) ([]models.SandboxSummary, error) {
	// Fetch all persisted sandboxes from the database.
	dbSandboxes, err := c.repo.FindAll()
	if err != nil {
		return nil, err
	}
	if len(dbSandboxes) == 0 {
		return []models.SandboxSummary{}, nil
	}

	// Fetch all containers (including stopped) to build a lookup map.
	result, err := c.cli.ContainerList(ctx, moby.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	type containerInfo struct {
		Name   string
		Image  string
		Status string
		State  string
		Ports  map[string]string
	}
	lookup := make(map[string]containerInfo, len(result.Items))
	for _, item := range result.Items {
		ports := make(map[string]string)
		for _, p := range item.Ports {
			if p.PublicPort > 0 {
				ports[portKey(p.PrivatePort, p.Type)] = portValue(p.PublicPort)
			}
		}
		lookup[item.ID] = containerInfo{
			Name:   containerName(item.Names),
			Image:  item.Image,
			Status: item.Status,
			State:  string(item.State),
			Ports:  ports,
		}
	}

	summaries := make([]models.SandboxSummary, 0, len(dbSandboxes))
	for _, db := range dbSandboxes {
		s := models.SandboxSummary{
			ID:    db.ID,
			Name:  db.Name,
			Image: db.Image,
			Ports: portKeys(map[string]string(db.Ports)),
		}

		// Enrich with live Docker state if the container still exists.
		if info, ok := lookup[db.ID]; ok {
			s.Name = info.Name
			s.Image = info.Image
			s.Status = info.Status
			s.State = info.State
			if len(info.Ports) > 0 {
				s.Ports = portKeys(info.Ports)
			}
		} else {
			s.Status = "removed"
			s.State = "removed"
		}

		// Attach expiration info if tracked.
		if entry := c.getTimerEntry(db.ID); entry != nil {
			ea := entry.expiresAt
			s.ExpiresAt = &ea
		}

		summaries = append(summaries, s)
	}

	return summaries, nil
}

// Create creates and starts a sandbox. Docker assigns host ports automatically.
// Applies optional resource limits and schedules auto-stop with a default TTL of 15 minutes.
// Returns ErrImageNotFound if the image does not exist locally.
func (c *Client) Create(ctx context.Context, req models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
	// Verify image exists locally
	exists, err := c.ImageExists(ctx, req.Image)
	if err != nil {
		return models.CreateSandboxResponse{}, err
	}
	if !exists {
		return models.CreateSandboxResponse{}, ErrImageNotFound
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
		return models.CreateSandboxResponse{}, err
	}

	if _, err := c.cli.ContainerStart(ctx, result.ID, moby.ContainerStartOptions{}); err != nil {
		return models.CreateSandboxResponse{}, err
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
		return models.CreateSandboxResponse{}, err
	}

	assignedPorts := extractPorts(info.Container.NetworkSettings.Ports)

	// Persist sandbox (fire-and-forget: log errors, don't block).
	if err := c.repo.Save(database.Sandbox{
		ID:    result.ID,
		Name:  name,
		Image: req.Image,
		Ports: database.JSONMap(assignedPorts),
		Port:  mainPort,
	}); err != nil {
		log.Printf("database: failed to persist sandbox %s: %v", result.ID, err)
	}

	return models.CreateSandboxResponse{
		ID:    result.ID,
		Name:  name,
		Ports: portKeys(assignedPorts),
	}, nil
}

// Inspect returns a curated view of a sandbox.
func (c *Client) Inspect(ctx context.Context, id string) (models.SandboxDetail, error) {
	result, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return models.SandboxDetail{}, wrapNotFound(err)
	}

	info := result.Container
	detail := models.SandboxDetail{
		ID:      info.ID,
		Name:    strings.TrimPrefix(info.Name, "/"),
		Image:   info.Config.Image,
		Status:  string(info.State.Status),
		Running: info.State.Running,
		Ports:   portKeys(extractPorts(info.NetworkSettings.Ports)),
		Resources: models.ResourceLimits{
			Memory: info.HostConfig.Memory / (1024 * 1024), // bytes to MB
			CPUs:   float64(info.HostConfig.NanoCPUs) / 1e9,
		},
		StartedAt:  info.State.StartedAt,
		FinishedAt: info.State.FinishedAt,
	}

	if entry := c.getTimerEntry(id); entry != nil {
		ea := entry.expiresAt
		detail.ExpiresAt = &ea
	}

	return detail, nil
}

// GetNetwork returns current exposed port mappings and selected main routing port.
func (c *Client) GetNetwork(ctx context.Context, id string) (models.SandboxNetwork, error) {
	sb, err := c.repo.FindByID(id)
	if err != nil {
		return models.SandboxNetwork{}, err
	}
	if sb == nil {
		return models.SandboxNetwork{}, ErrNotFound
	}

	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return models.SandboxNetwork{}, wrapNotFound(err)
	}

	ports := extractPorts(info.Container.NetworkSettings.Ports)
	mainPort := sb.Port
	if mainPort == "" && len(ports) == 1 {
		for p := range ports {
			mainPort = p
		}
	}

	return models.SandboxNetwork{MainPort: mainPort, PortsMap: ports}, nil
}

// Start starts a stopped sandbox and re-schedules the auto-stop timer.
// Returns ErrAlreadyRunning (409) if the sandbox is already running.
func (c *Client) Start(ctx context.Context, id string) (models.RestartResponse, error) {
	// Check current state to return a meaningful conflict error.
	pre, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return models.RestartResponse{}, wrapNotFound(err)
	}
	if pre.Container.State.Running {
		return models.RestartResponse{}, ErrAlreadyRunning
	}

	if _, err := c.cli.ContainerStart(ctx, id, moby.ContainerStartOptions{}); err != nil {
		return models.RestartResponse{}, wrapNotFound(err)
	}

	c.scheduleStop(id, defaultTimeout)

	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return models.RestartResponse{}, wrapNotFound(err)
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

	return models.RestartResponse{
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
		return ErrAlreadyStopped
	}

	c.cancelTimer(id)
	c.invalidateCache(id)
	_, err = c.cli.ContainerStop(ctx, id, moby.ContainerStopOptions{})
	return wrapNotFound(err)
}

// Restart restarts a sandbox and returns the new port mappings.
// It cancels any existing timer and schedules a fresh one with the default timeout.
func (c *Client) Restart(ctx context.Context, id string) (models.RestartResponse, error) {
	c.cancelTimer(id)

	if _, err := c.cli.ContainerRestart(ctx, id, moby.ContainerRestartOptions{}); err != nil {
		return models.RestartResponse{}, wrapNotFound(err)
	}

	// Re-schedule auto-stop with the default timeout.
	c.scheduleStop(id, defaultTimeout)

	// Inspect to get the new ports.
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return models.RestartResponse{}, wrapNotFound(err)
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

	return models.RestartResponse{
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
		return ErrAlreadyPaused
	}
	if !info.Container.State.Running {
		return ErrNotRunning
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
		return ErrNotPaused
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

// Stats returns a curated snapshot of container resource usage.
func (c *Client) Stats(ctx context.Context, id string) (models.SandboxStats, error) {
	result, err := c.cli.ContainerStats(ctx, id, moby.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	if err != nil {
		return models.SandboxStats{}, wrapNotFound(err)
	}
	defer result.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(result.Body).Decode(&raw); err != nil {
		return models.SandboxStats{}, fmt.Errorf("decode stats: %w", err)
	}

	// CPU % = (cpuDelta / systemDelta) * numCPUs * 100
	cpuPercent := 0.0
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage - raw.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(raw.CPUStats.SystemUsage - raw.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta >= 0 {
		cpuPercent = (cpuDelta / sysDelta) * float64(raw.CPUStats.OnlineCPUs) * 100.0
	}

	memPercent := 0.0
	if raw.MemoryStats.Limit > 0 {
		memPercent = float64(raw.MemoryStats.Usage) / float64(raw.MemoryStats.Limit) * 100.0
	}

	return models.SandboxStats{
		CPU: math.Round(cpuPercent*100) / 100, // 2 decimal places
		Memory: models.MemoryUsage{
			Usage:   raw.MemoryStats.Usage,
			Limit:   raw.MemoryStats.Limit,
			Percent: math.Round(memPercent*100) / 100,
		},
		PIDs: raw.PidsStats.Current,
	}, nil
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
func (c *Client) ExecCommand(ctx context.Context, sandboxID string, req models.ExecCommandRequest) (models.CommandDetail, error) {
	// Verify sandbox is running.
	info, err := c.cli.ContainerInspect(ctx, sandboxID, moby.ContainerInspectOptions{})
	if err != nil {
		return models.CommandDetail{}, wrapNotFound(err)
	}
	if !info.Container.State.Running {
		return models.CommandDetail{}, ErrNotRunning
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
		return models.CommandDetail{}, wrapNotFound(err)
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
		return models.CommandDetail{}, fmt.Errorf("save command: %w", err)
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
			rc.exitCode = -1
			rc.finished = true
			rc.mu.Unlock()
			c.repo.UpdateCommandFinished(cmdID, -1, time.Now().UnixMilli())
			return
		}
		defer attached.Close()

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

	return models.CommandDetail{
		ID:        cmdID,
		Name:      req.Command,
		Args:      req.Args,
		Cwd:       req.Cwd,
		SandboxID: sandboxID,
		StartedAt: now,
	}, nil
}

// GetCommand returns command details by ID.
func (c *Client) GetCommand(ctx context.Context, sandboxID, cmdID string) (models.CommandDetail, error) {
	dbCmd, err := c.repo.FindCommandByID(cmdID)
	if err != nil {
		return models.CommandDetail{}, err
	}
	if dbCmd == nil {
		return models.CommandDetail{}, ErrCommandNotFound
	}
	if dbCmd.SandboxID != sandboxID {
		return models.CommandDetail{}, ErrCommandNotFound
	}

	return c.dbCommandToDetail(*dbCmd), nil
}

// ListCommands returns all commands for a sandbox.
func (c *Client) ListCommands(ctx context.Context, sandboxID string) ([]models.CommandDetail, error) {
	// Verify sandbox exists.
	if _, err := c.cli.ContainerInspect(ctx, sandboxID, moby.ContainerInspectOptions{}); err != nil {
		return nil, wrapNotFound(err)
	}

	dbCmds, err := c.repo.FindCommandsBySandbox(sandboxID)
	if err != nil {
		return nil, err
	}

	details := make([]models.CommandDetail, 0, len(dbCmds))
	for _, cmd := range dbCmds {
		details = append(details, c.dbCommandToDetail(cmd))
	}
	return details, nil
}

// KillCommand sends a signal to a running command.
func (c *Client) KillCommand(ctx context.Context, sandboxID, cmdID string, signal int) (models.CommandDetail, error) {
	// Look up running command.
	v, ok := c.commands.Load(cmdID)
	if !ok {
		// Check if it exists in DB.
		dbCmd, err := c.repo.FindCommandByID(cmdID)
		if err != nil {
			return models.CommandDetail{}, err
		}
		if dbCmd == nil {
			return models.CommandDetail{}, ErrCommandNotFound
		}
		return models.CommandDetail{}, ErrCommandFinished
	}

	rc := v.(*runningCommand)
	rc.mu.Lock()
	if rc.finished {
		rc.mu.Unlock()
		return models.CommandDetail{}, ErrCommandFinished
	}
	if rc.sandboxID != sandboxID {
		rc.mu.Unlock()
		return models.CommandDetail{}, ErrCommandNotFound
	}
	cmd := rc.cmd
	rc.mu.Unlock()

	// Kill the process inside the container using pkill with the original command pattern.
	pattern := strings.Join(cmd, " ")
	killCmd := fmt.Sprintf("pkill -%d -f %q", signal, pattern)
	// Ignore error: pkill returns 1 if process already exited (race condition).
	c.execWithStdin(ctx, sandboxID, []string{"sh", "-c", killCmd}, nil)

	// Wait briefly for the command to finish, then return current state.
	select {
	case <-rc.done:
	case <-time.After(500 * time.Millisecond):
	}

	return c.GetCommand(ctx, sandboxID, cmdID)
}

// StreamCommandLogs returns readers for stdout and stderr of a command.
func (c *Client) StreamCommandLogs(ctx context.Context, sandboxID, cmdID string) (io.ReadCloser, io.ReadCloser, error) {
	v, ok := c.commands.Load(cmdID)
	if !ok {
		return nil, nil, ErrCommandNotFound
	}

	rc := v.(*runningCommand)
	if rc.sandboxID != sandboxID {
		return nil, nil, ErrCommandNotFound
	}

	return rc.stdout.NewReader(), rc.stderr.NewReader(), nil
}

// GetCommandLogs returns a snapshot of stdout and stderr for a command without streaming.
func (c *Client) GetCommandLogs(ctx context.Context, sandboxID, cmdID string) (models.CommandLogsResponse, error) {
	v, ok := c.commands.Load(cmdID)
	if !ok {
		return models.CommandLogsResponse{}, ErrCommandNotFound
	}

	rc := v.(*runningCommand)
	if rc.sandboxID != sandboxID {
		return models.CommandLogsResponse{}, ErrCommandNotFound
	}

	rc.mu.Lock()
	exitCode := (*int)(nil)
	if rc.finished {
		ec := rc.exitCode
		exitCode = &ec
	}
	rc.mu.Unlock()

	return models.CommandLogsResponse{
		Stdout:   string(rc.stdout.Bytes()),
		Stderr:   string(rc.stderr.Bytes()),
		ExitCode: exitCode,
	}, nil
}

// WaitCommand blocks until a command finishes and returns the updated detail.
func (c *Client) WaitCommand(ctx context.Context, sandboxID, cmdID string) (models.CommandDetail, error) {
	v, ok := c.commands.Load(cmdID)
	if !ok {
		// Already finished and cleaned up, or doesn't exist.
		return c.GetCommand(ctx, sandboxID, cmdID)
	}

	rc := v.(*runningCommand)
	select {
	case <-rc.done:
	case <-ctx.Done():
		return models.CommandDetail{}, ctx.Err()
	}

	return c.GetCommand(ctx, sandboxID, cmdID)
}

// dbCommandToDetail converts a database.Command to models.CommandDetail.
func (c *Client) dbCommandToDetail(cmd database.Command) models.CommandDetail {
	var args []string
	if cmd.Args != "" {
		json.Unmarshal([]byte(cmd.Args), &args)
	}

	detail := models.CommandDetail{
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

// ReadFile reads the content of a file inside a sandbox.
func (c *Client) ReadFile(ctx context.Context, id, path string) (string, error) {
	result, err := c.execWithStdin(ctx, id, []string{"cat", path}, nil)
	if err != nil {
		return "", err
	}
	return result.stdout, nil
}

// WriteFile writes content to a file inside a sandbox (creates parent dirs as needed).
func (c *Client) WriteFile(ctx context.Context, id, path, content string) error {
	if _, err := c.execWithStdin(ctx, id, []string{"sh", "-c", "mkdir -p $(dirname '" + path + "')"}, nil); err != nil {
		return err
	}
	_, err := c.execWithStdin(ctx, id, []string{"sh", "-c", "cat > '" + path + "'"}, strings.NewReader(content))
	return err
}

// DeleteFile deletes a file or directory inside a sandbox.
func (c *Client) DeleteFile(ctx context.Context, id, path string) error {
	_, err := c.execWithStdin(ctx, id, []string{"rm", "-rf", path}, nil)
	return err
}

// ListDir lists the contents of a directory inside a sandbox.
func (c *Client) ListDir(ctx context.Context, id, path string) (string, error) {
	result, err := c.execWithStdin(ctx, id, []string{"ls", "-la", path}, nil)
	if err != nil {
		return "", err
	}
	return result.stdout, nil
}

// PullImage pulls a Docker image from a registry and waits for completion.
// It reads the JSON message stream to detect errors that the Docker daemon
// reports inline (e.g. "no matching manifest for linux/amd64").
func (c *Client) PullImage(ctx context.Context, image string) error {
	resp, err := c.cli.ImagePull(ctx, image, moby.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()

	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if msg.Error != nil {
			return fmt.Errorf("pull %s: %s", image, msg.Error.Message)
		}
	}

	// Verify the image actually exists locally after pull.
	if exists, err := c.ImageExists(ctx, image); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("pull %s: image not available after pull", image)
	}

	return nil
}

// RemoveImage removes a local Docker image. Use force=true to remove even if containers reference it.
func (c *Client) RemoveImage(ctx context.Context, id string, force bool) error {
	_, err := c.cli.ImageRemove(ctx, id, moby.ImageRemoveOptions{
		Force:         force,
		PruneChildren: true,
	})
	if err != nil {
		return wrapNotFound(err)
	}
	return nil
}

// InspectImage returns curated details for a single Docker image.
func (c *Client) InspectImage(ctx context.Context, id string) (models.ImageDetail, error) {
	result, err := c.cli.ImageInspect(ctx, id)
	if err != nil {
		return models.ImageDetail{}, wrapNotFound(err)
	}

	return models.ImageDetail{
		ID:           result.ID,
		Tags:         result.RepoTags,
		Size:         result.Size,
		Created:      result.Created,
		Architecture: result.Architecture,
		OS:           result.Os,
	}, nil
}

// ListImages returns all locally available Docker images.
func (c *Client) ListImages(ctx context.Context) ([]models.ImageSummary, error) {
	result, err := c.cli.ImageList(ctx, moby.ImageListOptions{})
	if err != nil {
		return nil, err
	}

	images := make([]models.ImageSummary, 0, len(result.Items))
	for _, item := range result.Items {
		images = append(images, models.ImageSummary{
			ID:   item.ID,
			Tags: item.RepoTags,
			Size: item.Size,
		})
	}
	return images, nil
}

// ImageExists checks if an image exists locally.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := c.cli.ImageInspect(ctx, image)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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

	if stdin != nil {
		if _, err := io.Copy(attached.Conn, stdin); err != nil {
			return execResult{}, err
		}
		attached.CloseWrite()
	}

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil && err != io.EOF {
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

// wrapNotFound converts Docker "not found" errors to ErrNotFound.
func wrapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errdefs.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// normalizePort ensures a port spec has a protocol suffix.
// "3000" → "3000/tcp", "3000/tcp" → "3000/tcp" (unchanged).
func normalizePort(port string) string {
	if port == "" {
		return ""
	}
	if !strings.Contains(port, "/") {
		return port + "/tcp"
	}
	return port
}

// normalizePorts normalizes a slice of port specs.
func normalizePorts(ports []string) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if n := normalizePort(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// buildExposedPorts converts a slice of port specs to network.PortSet.
func buildExposedPorts(ports []string) network.PortSet {
	if len(ports) == 0 {
		return nil
	}
	ps := make(network.PortSet)
	for _, p := range ports {
		parsed, err := network.ParsePort(p)
		if err != nil {
			continue
		}
		ps[parsed] = struct{}{}
	}
	if len(ps) == 0 {
		return nil
	}
	return ps
}

// buildPortBindings creates port bindings that only listen on 127.0.0.1 (loopback).
// This ensures container ports are only reachable through the reverse proxy, not directly.
func buildPortBindings(ports []string) network.PortMap {
	if len(ports) == 0 {
		return nil
	}
	pm := make(network.PortMap)
	for _, p := range ports {
		parsed, err := network.ParsePort(p)
		if err != nil {
			continue
		}
		pm[parsed] = []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}}
	}
	if len(pm) == 0 {
		return nil
	}
	return pm
}

// extractPorts converts network.PortMap to map["80/tcp"]"32768".
func extractPorts(pm network.PortMap) map[string]string {
	out := make(map[string]string)
	for port, bindings := range pm {
		if len(bindings) > 0 {
			out[port.String()] = bindings[0].HostPort
		}
	}
	return out
}

// portKeys returns the container port keys from a port map (e.g. ["3000/tcp", "8080/tcp"]).
func portKeys(pm map[string]string) []string {
	keys := make([]string, 0, len(pm))
	for k := range pm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// containerName extracts a clean name from Docker's name list (removes leading /).
func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// portKey builds a port key like "3000/tcp".
func portKey(port uint16, proto string) string {
	if proto == "" {
		proto = "tcp"
	}
	return portValue(port) + "/" + proto
}

// portValue converts a uint16 port to its string representation.
func portValue(port uint16) string {
	return fmt.Sprintf("%d", port)
}

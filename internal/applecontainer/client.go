package applecontainer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

const defaultTimeout = 900
const ownerLabel = "io.opensbx.managed"

type expiration struct {
	timer *time.Timer
	at    time.Time
}

type Client struct {
	runner     Runner
	repo       *database.Repository
	now        func() time.Time
	mu         sync.Mutex // Serializes lifecycle mutations, port allocation and TTL callbacks.
	timers     map[string]*expiration
	finished   map[string]string
	commands   map[string]*runningCommand
	closing    bool
	invalidate func(string)
}

// New accepts an injectable runner and clock. Call Ping before opening listeners.
func New(repo *database.Repository, runner Runner, now func() time.Time) *Client {
	if now == nil {
		now = time.Now
	}
	return &Client{repo: repo.NativeView(), runner: runner, now: now, timers: make(map[string]*expiration), finished: make(map[string]string), commands: make(map[string]*runningCommand)}
}
func (c *Client) SetCacheInvalidator(fn func(string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidate = fn
}
func (c *Client) changed(id string) {
	if c.invalidate != nil {
		c.invalidate(id)
	}
}

func (c *Client) Ping(ctx context.Context) error {
	b, err := c.run(ctx, nil, "system", "status", "--format", "json")
	if err != nil {
		return fmt.Errorf("Apple Container service is unavailable. Start it with `container system start`; install the CLI from https://github.com/apple/container/releases if needed: %w", err)
	}
	var status struct {
		Status string
		Client struct{ Version string }
		Server struct{ Version string }
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return fmt.Errorf("invalid Apple container health JSON: %w", err)
	}
	if status.Status != "running" {
		return fmt.Errorf("Apple Container service is not running (status %q); start it with `container system start`", status.Status)
	}
	// CLI JSON is not a stable API. Fail closed on unvalidated schema versions.
	if status.Client.Version != "1.4.1" || status.Server.Version != "1.4.1" {
		return errors.New("Apple Container CLI and service version 1.4.1 are required. Install the matching signed release from https://github.com/apple/container/releases, then run `container system start`")
	}
	return nil
}

type containerInfo struct {
	Configuration struct {
		ID        string
		Labels    map[string]string
		Image     struct{ Reference string }
		Resources struct {
			CPUs          float64
			MemoryInBytes int64
		}
		PublishedPorts []struct {
			HostPort      int
			ContainerPort int
			Proto         string
			Count         int
		}
	}
	Status struct {
		State       string
		StartedDate json.RawMessage
	}
}

func (c *Client) owned(id string) (*database.Sandbox, error) {
	if !validID(id) {
		return nil, sandbox.ErrNotFound
	}
	sb, err := c.repo.FindByID(id)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		return nil, sandbox.ErrNotFound
	}
	return sb, nil
}
func validID(id string) bool {
	if !strings.HasPrefix(id, "opensbx-") || len(id) != len("opensbx-")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "opensbx-"))
	return err == nil
}
func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func (c *Client) lookup(ctx context.Context, id string) (containerInfo, error) {
	if _, err := c.owned(id); err != nil {
		return containerInfo{}, err
	}
	inventory, err := c.inventory(ctx)
	if err != nil {
		return containerInfo{}, err
	}
	return ownedInventoryEntry(inventory, id)
}

func (c *Client) inventory(ctx context.Context) (map[string]containerInfo, error) {
	// List is read-only and permits exact missing-resource detection without
	// guessing from localized stderr. Never mutate resources absent from the DB.
	b, err := c.run(ctx, nil, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var all []containerInfo
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("invalid container list JSON: %w", err)
	}
	inventory := make(map[string]containerInfo, len(all))
	for _, info := range all {
		if _, exists := inventory[info.Configuration.ID]; !exists {
			inventory[info.Configuration.ID] = info
		}
	}
	return inventory, nil
}

// The caller must first obtain the ID from the selected repository. Validate
// only that entry: unrelated containers in the inventory are never adopted.
func ownedInventoryEntry(inventory map[string]containerInfo, id string) (containerInfo, error) {
	info, found := inventory[id]
	if !validID(id) || !found {
		return containerInfo{}, sandbox.ErrNotFound
	}
	if info.Configuration.Labels[ownerLabel] != id {
		return containerInfo{}, errors.New("container ownership label does not match repository")
	}
	if info.Configuration.Resources.CPUs <= 0 || info.Configuration.Resources.MemoryInBytes <= 0 {
		return containerInfo{}, errors.New("incomplete Apple container resource JSON")
	}
	switch info.Status.State {
	case "running", "stopped", "stopping", "unknown":
	default:
		return containerInfo{}, errors.New("unsupported container state in JSON")
	}
	return info, nil
}

func dateString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var seconds float64
	if len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &seconds) == nil {
		// Foundation's default Date encoding is seconds since 2001-01-01.
		return time.Unix(978307200+int64(seconds), int64((seconds-math.Floor(seconds))*1e9)).UTC().Format(time.RFC3339Nano)
	}
	return ""
}
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func networkPorts(info containerInfo) (map[string]string, error) {
	ports := make(map[string]string, len(info.Configuration.PublishedPorts))
	for _, p := range info.Configuration.PublishedPorts {
		if p.HostPort < 2 || p.HostPort > 65535 || p.ContainerPort < 2 || p.ContainerPort > 65535 || (p.Proto != "tcp" && p.Proto != "udp") || p.Count > 1 {
			return nil, errors.New("unexpected Apple published port configuration")
		}
		ports[strconv.Itoa(p.ContainerPort)+"/"+p.Proto] = strconv.Itoa(p.HostPort)
	}
	return ports, nil
}

type sandboxTiming struct {
	expiresAt  *time.Time
	finishedAt string
}

func (c *Client) timingLocked(id string) sandboxTiming {
	timing := sandboxTiming{finishedAt: c.finished[id]}
	if e := c.timers[id]; e != nil {
		at := e.at
		timing.expiresAt = &at
	}
	return timing
}

func sandboxDetail(sb database.Sandbox, info containerInfo, timing sandboxTiming) (runtimeio.SandboxDetail, error) {
	ports, err := networkPorts(info)
	if err != nil {
		return runtimeio.SandboxDetail{}, err
	}
	d := runtimeio.SandboxDetail{ID: sb.ID, Name: sb.Name, Image: sb.Image, Status: info.Status.State, Running: info.Status.State == "running", Ports: keys(ports), Resources: runtimeio.ResourceLimits{CPUs: info.Configuration.Resources.CPUs, Memory: info.Configuration.Resources.MemoryInBytes / (1024 * 1024)}, StartedAt: dateString(info.Status.StartedDate), FinishedAt: timing.finishedAt, ExpiresAt: timing.expiresAt}
	return d, nil
}

func (c *Client) detailLocked(ctx context.Context, id string) (runtimeio.SandboxDetail, error) {
	info, err := c.lookup(ctx, id)
	if err != nil {
		return runtimeio.SandboxDetail{}, err
	}
	sb, err := c.owned(id)
	if err != nil {
		return runtimeio.SandboxDetail{}, err
	}
	return sandboxDetail(*sb, info, c.timingLocked(id))
}
func (c *Client) Inspect(ctx context.Context, id string) (runtimeio.SandboxDetail, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.detailLocked(ctx, id)
}
func (c *Client) List(ctx context.Context) ([]runtimeio.SandboxSummary, error) {
	rows, err := c.repo.FindAll()
	if err != nil {
		return nil, err
	}
	result := make([]runtimeio.SandboxSummary, 0, len(rows))
	if len(rows) == 0 {
		return result, nil
	}
	inventory, err := c.inventory(ctx)
	if err != nil {
		return nil, err
	}
	// Snapshot only process-local timing under the lifecycle mutex. Neither
	// the CLI inventory nor reconciliation should block Stop or TTL callbacks.
	timings := make(map[string]sandboxTiming, len(rows))
	c.mu.Lock()
	for _, row := range rows {
		timings[row.ID] = c.timingLocked(row.ID)
	}
	c.mu.Unlock()
	for _, row := range rows {
		info, err := ownedInventoryEntry(inventory, row.ID)
		if errors.Is(err, sandbox.ErrNotFound) {
			result = append(result, runtimeio.SandboxSummary{ID: row.ID, Name: row.Name, Image: row.Image, Status: "removed", State: "removed", Ports: keys(row.Ports)})
			continue
		}
		if err != nil {
			return nil, err
		}
		d, err := sandboxDetail(row, info, timings[row.ID])
		if err != nil {
			return nil, err
		}
		result = append(result, runtimeio.SandboxSummary{ID: d.ID, Name: d.Name, Image: d.Image, Status: d.Status, State: d.Status, Ports: d.Ports, ExpiresAt: d.ExpiresAt})
	}
	return result, nil
}

func normalizePorts(ports []string) ([]string, error) {
	result := make([]string, 0, len(ports))
	seen := map[string]bool{}
	if len(ports) > 128 {
		return nil, errors.New("too many published ports (maximum 128)")
	}
	for _, p := range ports {
		parts := strings.Split(p, "/")
		if len(parts) > 2 {
			return nil, errors.New("invalid container port")
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil || n < 2 || n > 65535 {
			return nil, errors.New("Apple container ports must be between 2 and 65535")
		}
		proto := "tcp"
		if len(parts) == 2 {
			proto = parts[1]
		}
		if proto != "tcp" && proto != "udp" {
			return nil, errors.New("port protocol must be tcp or udp")
		}
		key := strconv.Itoa(n) + "/" + proto
		if !seen[key] {
			result = append(result, key)
			seen[key] = true
		}
	}
	return result, nil
}
func validEnv(key, value string) bool {
	if key == "" || strings.ContainsRune(value, 0) {
		return false
	}
	for i, r := range key {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
func envArgs(env []string) ([]string, error) {
	args := []string{}
	var budget inputBudget
	for _, s := range env {
		if err := budget.add(s); err != nil {
			return nil, err
		}
		key, value, ok := strings.Cut(s, "=")
		if !ok || !validEnv(key, value) {
			return nil, errors.New("environment entries must be explicit KEY=VALUE pairs")
		}
		args = append(args, "--env", key+"="+value)
	}
	return args, nil
}

// reservePorts holds loopback sockets while allocating the whole set. They must
// be closed before the runtime binds: this is NOT an atomic reservation across
// processes. Create/start failures are rolled back and retried with fresh ports.
func reservePorts(ports []string) (map[string]string, func(), error) {
	closers := []interface{ Close() error }{}
	release := func() {
		for _, s := range closers {
			_ = s.Close()
		}
	}
	m := map[string]string{}
	for _, p := range ports {
		if strings.HasSuffix(p, "/udp") {
			s, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				release()
				return nil, nil, err
			}
			closers = append(closers, s)
			m[p] = strconv.Itoa(s.LocalAddr().(*net.UDPAddr).Port)
		} else {
			s, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				release()
				return nil, nil, err
			}
			closers = append(closers, s)
			m[p] = strconv.Itoa(s.Addr().(*net.TCPAddr).Port)
		}
	}
	return m, release, nil
}

func (c *Client) Create(ctx context.Context, req runtimeio.CreateSandboxRequest) (runtimeio.CreateSandboxResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result runtimeio.CreateSandboxResponse
	if c.closing {
		return result, errors.New("backend is shutting down")
	}
	ports, err := normalizePorts(req.Ports)
	if err != nil {
		return result, err
	}
	env, err := envArgs(req.Env)
	if err != nil {
		return result, err
	}
	mem, cpus := int64(1024), float64(1)
	if req.Resources != nil {
		if req.Resources.Memory != 0 {
			mem = req.Resources.Memory
		}
		if req.Resources.CPUs != 0 {
			cpus = req.Resources.CPUs
		}
	}
	if mem < 1 || mem > 8192 {
		return result, errors.New("memory must be between 1 and 8192 MiB")
	}
	if math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus < 1 || cpus > 4 || math.Trunc(cpus) != cpus {
		return result, errors.New("Apple container requires whole CPUs between 1 and 4; fractional CPUs are unsupported")
	}
	if req.Timeout < 0 || int64(req.Timeout) > int64(math.MaxInt64/int64(time.Second)) {
		return result, errors.New("invalid sandbox timeout")
	}
	image, err := c.findImage(ctx, req.Image)
	if err != nil {
		return result, err
	}
	if _, err := nativeVariant(image); err != nil {
		return result, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		id, err := randomID("opensbx-")
		if err != nil {
			return result, err
		}
		mapped, release, err := reservePorts(ports)
		if err != nil {
			return result, err
		}
		// In validated 1.4.1, fetch returns local content first; pull rejects zero
		// downloads before creating any registry request. This also applies to
		// Apple's configured vminit image, which must already be prepared locally.
		args := []string{"create", "--max-concurrent-downloads", "0", "--name", id, "--label", ownerLabel + "=" + id, "--cpus", strconv.Itoa(int(cpus)), "--memory", strconv.FormatInt(mem, 10) + "MiB", "--platform", "linux/arm64", "--entrypoint", "/bin/sh"}
		args = append(args, env...)
		for _, p := range ports {
			args = append(args, "--publish", "127.0.0.1:"+mapped[p]+":"+p)
		}
		// Pin the already-local reference; do not pass unvalidated user flags.
		args = append(args, image.Configuration.Name, "-c", "exec sleep infinity")
		release()
		_, createErr := c.run(ctx, nil, args...)
		if createErr == nil {
			_, createErr = c.run(ctx, nil, "start", id)
		}
		if createErr != nil {
			// Preserve a recovery record if rollback cannot complete. Only this
			// random, app-labelled resource can be touched by cleanup.
			row := database.Sandbox{ID: sandbox.CreationID(ctx, id), NativeID: id, Name: sandbox.CreationID(ctx, id), Image: sandbox.CreationImage(ctx, req.Image), Ports: database.JSONMap(mapped)}
			if len(ports) > 0 {
				row.Port = ports[0]
			}
			if err := c.rollback(id); err != nil {
				saveErr := c.repo.CreateOwnership(row)
				return result, errors.Join(createErr, err, saveErr)
			}
			lastErr = createErr
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if errors.Is(createErr, errOfflineImageUnavailable) {
				return result, createErr
			}
			continue
		}
		row := database.Sandbox{ID: sandbox.CreationID(ctx, id), NativeID: id, Name: sandbox.CreationID(ctx, id), Image: sandbox.CreationImage(ctx, req.Image), Ports: database.JSONMap(mapped)}
		if len(ports) > 0 {
			row.Port = ports[0]
		}
		if err := c.repo.CreateOwnership(row); err != nil {
			rollbackErr := c.rollback(id)
			if rollbackErr == nil {
				return result, err
			}
			// No Provisioned handle reaches the service on this path. Retain this
			// exact creation's identity without overwriting a colliding owner row.
			cause := errors.Join(err, rollbackErr)
			row.ImageRoot = sandbox.CreationImage(ctx, "")
			row.NativeImage = image.Configuration.Name
			if ref, refErr := imageRef(image.Configuration.Name); refErr == nil {
				_, row.ImageManifest, _ = strings.Cut(ref, "@")
			}
			row.RecoveryError = cause.Error()
			return result, errors.Join(cause, c.repo.CreateOwnership(row))
		}
		c.scheduleLocked(id, req.Timeout)
		return runtimeio.CreateSandboxResponse{ID: id, Name: id, Ports: ports}, nil
	}
	return result, fmt.Errorf("Apple container create/start failed after 3 attempts: %w", lastErr)
}

func (c *Client) rollback(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The random ID is generated by this Create attempt, never caller supplied.
	b, err := c.run(ctx, nil, "list", "--all", "--format", "json")
	if err != nil {
		return err
	}
	var all []containerInfo
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, info := range all {
		if info.Configuration.ID == id {
			if info.Configuration.Labels[ownerLabel] != id {
				return errors.New("rollback ownership mismatch")
			}
			_, err = c.run(ctx, nil, "delete", "--force", id)
			return err
		}
	}
	return nil
}

func (c *Client) scheduleLocked(id string, timeout int) {
	if e := c.timers[id]; e != nil {
		e.timer.Stop()
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	e := &expiration{at: c.now().Add(time.Duration(timeout) * time.Second)}
	e.timer = time.AfterFunc(time.Duration(timeout)*time.Second, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.timers[id] != e || c.closing {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.stopLocked(ctx, id); err != nil && !errors.Is(err, sandbox.ErrAlreadyStopped) && !errors.Is(err, sandbox.ErrNotFound) {
			log.Printf("Apple sandbox TTL stop failed for %s: %v", id, err)
			c.scheduleLocked(id, 30)
		}
	})
	c.timers[id] = e
}
func (c *Client) clearTimer(id string) {
	if e := c.timers[id]; e != nil {
		e.timer.Stop()
		delete(c.timers, id)
	}
}
func (c *Client) stopLocked(ctx context.Context, id string) error {
	info, err := c.lookup(ctx, id)
	if err != nil {
		return err
	}
	if info.Status.State == "stopped" {
		c.clearTimer(id)
		c.changed(id)
		return sandbox.ErrAlreadyStopped
	}
	if _, err := c.run(ctx, nil, "stop", id); err != nil {
		return err
	}
	c.clearTimer(id)
	c.finished[id] = c.now().UTC().Format(time.RFC3339Nano)
	c.changed(id)
	return nil
}
func (c *Client) Stop(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopLocked(ctx, id)
}
func (c *Client) startLocked(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	if c.closing {
		return runtimeio.RestartResponse{}, errors.New("backend is shutting down")
	}
	info, err := c.lookup(ctx, id)
	if err != nil {
		return runtimeio.RestartResponse{}, err
	}
	if info.Status.State == "running" {
		return runtimeio.RestartResponse{}, sandbox.ErrAlreadyRunning
	}
	ports, err := networkPorts(info)
	if err != nil {
		return runtimeio.RestartResponse{}, err
	}
	if _, err := c.run(ctx, nil, "start", id); err != nil {
		return runtimeio.RestartResponse{}, err
	}
	delete(c.finished, id)
	c.scheduleLocked(id, defaultTimeout)
	c.changed(id)
	if err := c.repo.UpdatePorts(id, database.JSONMap(ports)); err != nil {
		return runtimeio.RestartResponse{}, err
	}
	d, err := c.detailLocked(ctx, id)
	return runtimeio.RestartResponse{Status: "started", Ports: d.Ports, ExpiresAt: d.ExpiresAt}, err
}
func (c *Client) Start(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked(ctx, id)
}
func (c *Client) Restart(ctx context.Context, id string) (runtimeio.RestartResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.stopLocked(ctx, id); err != nil && !errors.Is(err, sandbox.ErrAlreadyStopped) {
		return runtimeio.RestartResponse{}, err
	}
	// Never recreate an existing sandbox to repair a port collision.
	result, err := c.startLocked(ctx, id)
	if err == nil {
		result.Status = "restarted"
	}
	return result, err
}
func (c *Client) GetNetwork(ctx context.Context, id string) (runtimeio.SandboxNetwork, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := c.lookup(ctx, id)
	if err != nil {
		return runtimeio.SandboxNetwork{}, err
	}
	sb, err := c.owned(id)
	if err != nil {
		return runtimeio.SandboxNetwork{}, err
	}
	ports, err := networkPorts(info)
	if err != nil {
		return runtimeio.SandboxNetwork{}, err
	}
	return runtimeio.SandboxNetwork{MainPort: sb.Port, PortsMap: ports}, nil
}
func (c *Client) Remove(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.owned(id); err != nil {
		return err
	}
	_, err := c.lookup(ctx, id)
	if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	if err == nil {
		if _, err = c.run(ctx, nil, "delete", "--force", id); err != nil {
			return err
		}
	}
	c.clearTimer(id)
	c.changed(id)
	if err := c.repo.DeleteCommandsBySandbox(id); err != nil {
		return err
	}
	if err := c.repo.Delete(id); err != nil {
		return err
	}
	delete(c.finished, id)
	for cmdID, cmd := range c.commands {
		if cmd.sandboxID == id {
			// Guest deletion was confirmed above; release any CLI still waiting
			// on its transport. This is not used as a guest command kill.
			select {
			case <-cmd.done:
			default:
				_ = cmd.process.Kill()
			}
			delete(c.commands, cmdID)
		}
	}
	return nil
}
func (c *Client) Pause(ctx context.Context, id string) error {
	if _, err := c.owned(id); err != nil {
		return err
	}
	return errors.New("pause is unsupported by Apple container")
}
func (c *Client) Resume(ctx context.Context, id string) error {
	if _, err := c.owned(id); err != nil {
		return err
	}
	return errors.New("resume is unsupported by Apple container")
}
func (c *Client) RenewExpiration(ctx context.Context, id string, timeout int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return errors.New("backend is shutting down")
	}
	if timeout <= 0 || int64(timeout) > int64(math.MaxInt64/int64(time.Second)) {
		return errors.New("invalid sandbox timeout")
	}
	if _, err := c.lookup(ctx, id); err != nil {
		return err
	}
	c.scheduleLocked(id, timeout)
	return nil
}
func (c *Client) Shutdown(ctx context.Context) {
	// A long-running API mutation must not make shutdown exceed its budget
	// merely waiting to acquire the lifecycle lock.
	for !c.mu.TryLock() {
		select {
		case <-ctx.Done():
			log.Printf("Apple shutdown could not acquire lifecycle lock before deadline; sandbox stop is unconfirmed")
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer c.mu.Unlock()
	c.closing = true
	for id := range c.timers {
		c.clearTimer(id)
	}
	rows, err := c.repo.FindAll()
	if err != nil {
		log.Printf("Apple shutdown repository: %v", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		if err := c.stopLocked(ctx, row.ID); err != nil && !errors.Is(err, sandbox.ErrAlreadyStopped) && !errors.Is(err, sandbox.ErrNotFound) {
			log.Printf("Apple shutdown %s: %v", row.ID, err)
		}
	}
	for _, cmd := range c.commands {
		select {
		case <-cmd.done:
		default:
			_ = cmd.process.Kill()
		}
	}
}

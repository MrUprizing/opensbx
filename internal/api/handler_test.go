package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"opensbx/internal/api"
	"opensbx/internal/sandbox"
	"opensbx/models"
)

func init() { gin.SetMode(gin.TestMode) }

// stub implements sandbox.Application for testing without a native runtime.
// Each field is an optional function — set only what the test needs, leave the rest nil.
// If a nil method is called unexpectedly the test will panic, making the gap obvious.
type stub struct {
	ping              func() error
	list              func() ([]models.SandboxSummary, error)
	create            func(models.CreateSandboxRequest) (models.CreateSandboxResponse, error)
	inspect           func(string) (models.SandboxDetail, error)
	start             func(string) (models.RestartResponse, error)
	stop              func(string) error
	restart           func(string) (models.RestartResponse, error)
	getNetwork        func(string) (models.SandboxNetwork, error)
	remove            func(string) error
	pause             func(string) error
	resume            func(string) error
	renewExpiration   func(string, int) error
	execCommand       func(string, models.ExecCommandRequest) (models.CommandDetail, error)
	getCommand        func(string, string) (models.CommandDetail, error)
	listCommands      func(string) ([]models.CommandDetail, error)
	killCommand       func(string, string, int) (models.CommandDetail, error)
	streamCommandLogs func(string, string) (io.ReadCloser, io.ReadCloser, error)
	getCommandLogs    func(string, string) (models.CommandLogsResponse, error)
	waitCommand       func(string, string) (models.CommandDetail, error)
	stats             func(string) (models.SandboxStats, error)
	readFile          func(string, string) (string, error)
	writeFile         func(string, string, string) error
	deleteFile        func(string, string) error
	listDir           func(string, string) (string, error)
	pullImage         func(string) error
	removeImage       func(string, bool) error
	inspectImage      func(string) (models.ImageDetail, error)
	listImages        func() ([]models.ImageSummary, error)
}

func (s *stub) Ping(_ context.Context) error {
	if s.ping != nil {
		return s.ping()
	}
	return nil
}
func (s *stub) List(_ context.Context) ([]models.SandboxSummary, error) {
	return s.list()
}
func (s *stub) Create(_ context.Context, r models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
	return s.create(r)
}
func (s *stub) Inspect(_ context.Context, id string) (models.SandboxDetail, error) {
	return s.inspect(id)
}
func (s *stub) Start(_ context.Context, id string) (models.RestartResponse, error) {
	if s.start != nil {
		return s.start(id)
	}
	return models.RestartResponse{}, nil
}
func (s *stub) Stop(_ context.Context, id string) error { return s.stop(id) }
func (s *stub) Restart(_ context.Context, id string) (models.RestartResponse, error) {
	return s.restart(id)
}
func (s *stub) GetNetwork(_ context.Context, id string) (models.SandboxNetwork, error) {
	if s.getNetwork != nil {
		return s.getNetwork(id)
	}
	return models.SandboxNetwork{}, nil
}
func (s *stub) Remove(_ context.Context, id string) error { return s.remove(id) }
func (s *stub) Pause(_ context.Context, id string) error  { return s.pause(id) }
func (s *stub) Resume(_ context.Context, id string) error { return s.resume(id) }
func (s *stub) RenewExpiration(_ context.Context, id string, timeout int) error {
	return s.renewExpiration(id, timeout)
}
func (s *stub) ExecCommand(_ context.Context, sandboxID string, req models.ExecCommandRequest) (models.CommandDetail, error) {
	if s.execCommand != nil {
		return s.execCommand(sandboxID, req)
	}
	return models.CommandDetail{}, nil
}
func (s *stub) GetCommand(_ context.Context, sandboxID, cmdID string) (models.CommandDetail, error) {
	if s.getCommand != nil {
		return s.getCommand(sandboxID, cmdID)
	}
	return models.CommandDetail{}, nil
}
func (s *stub) ListCommands(_ context.Context, sandboxID string) ([]models.CommandDetail, error) {
	if s.listCommands != nil {
		return s.listCommands(sandboxID)
	}
	return []models.CommandDetail{}, nil
}
func (s *stub) KillCommand(_ context.Context, sandboxID, cmdID string, signal int) (models.CommandDetail, error) {
	if s.killCommand != nil {
		return s.killCommand(sandboxID, cmdID, signal)
	}
	return models.CommandDetail{}, nil
}
func (s *stub) StreamCommandLogs(_ context.Context, sandboxID, cmdID string) (io.ReadCloser, io.ReadCloser, error) {
	if s.streamCommandLogs != nil {
		return s.streamCommandLogs(sandboxID, cmdID)
	}
	return io.NopCloser(bytes.NewReader(nil)), io.NopCloser(bytes.NewReader(nil)), nil
}
func (s *stub) GetCommandLogs(_ context.Context, sandboxID, cmdID string) (models.CommandLogsResponse, error) {
	if s.getCommandLogs != nil {
		return s.getCommandLogs(sandboxID, cmdID)
	}
	return models.CommandLogsResponse{}, nil
}
func (s *stub) WaitCommand(_ context.Context, sandboxID, cmdID string) (models.CommandDetail, error) {
	if s.waitCommand != nil {
		return s.waitCommand(sandboxID, cmdID)
	}
	return models.CommandDetail{}, nil
}
func (s *stub) Stats(_ context.Context, id string) (models.SandboxStats, error) {
	if s.stats != nil {
		return s.stats(id)
	}
	return models.SandboxStats{}, nil
}
func (s *stub) ReadFile(_ context.Context, id, path string) (string, error) {
	return s.readFile(id, path)
}
func (s *stub) WriteFile(_ context.Context, id, path, content string) error {
	return s.writeFile(id, path, content)
}
func (s *stub) DeleteFile(_ context.Context, id, path string) error { return s.deleteFile(id, path) }
func (s *stub) ListDir(_ context.Context, id, path string) (string, error) {
	return s.listDir(id, path)
}
func (s *stub) PullImage(_ context.Context, image string) error {
	if s.pullImage != nil {
		return s.pullImage(image)
	}
	return nil
}
func (s *stub) RemoveImage(_ context.Context, id string, force bool) error {
	if s.removeImage != nil {
		return s.removeImage(id, force)
	}
	return nil
}
func (s *stub) InspectImage(_ context.Context, id string) (models.ImageDetail, error) {
	if s.inspectImage != nil {
		return s.inspectImage(id)
	}
	return models.ImageDetail{}, nil
}
func (s *stub) ListImages(_ context.Context) ([]models.ImageSummary, error) {
	if s.listImages != nil {
		return s.listImages()
	}
	return []models.ImageSummary{}, nil
}

// domainStub keeps REST contract fixtures at the DTO edge while the handler
// itself consumes only the runtime-neutral application port.
type domainStub struct {
	dto  *stub
	wait func(context.Context, sandbox.SandboxID, sandbox.CommandID) (sandbox.Command, error)
}

func parseDomainPorts(raw []string) []sandbox.Port {
	ports, err := sandbox.ParsePorts(raw)
	if err != nil {
		panic(err)
	}
	return ports
}
func (s *domainStub) Ping(ctx context.Context) error { return s.dto.Ping(ctx) }
func (s *domainStub) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Created, error) {
	request := models.CreateSandboxRequest{Image: opts.Image, Ports: sandbox.PortStrings(opts.Ports), Timeout: int(opts.Timeout / time.Second), Env: opts.Env}
	if opts.Resources != (sandbox.ResourceLimits{}) {
		request.Resources = &models.ResourceLimits{Memory: opts.Resources.MemoryMB, CPUs: opts.Resources.CPUs}
	}
	x, err := s.dto.Create(ctx, request)
	return sandbox.Created{ID: sandbox.SandboxID(x.ID), Name: x.Name, Ports: parseDomainPorts(x.Ports), URL: x.URL}, err
}
func (s *domainStub) List(ctx context.Context) ([]sandbox.Summary, error) {
	items, err := s.dto.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.Summary, 0, len(items))
	for _, x := range items {
		out = append(out, sandbox.Summary{ID: sandbox.SandboxID(x.ID), Name: x.Name, Image: sandbox.ImageID(x.Image), Status: x.Status, State: x.State, Ports: parseDomainPorts(x.Ports), ExpiresAt: x.ExpiresAt, URL: x.URL})
	}
	return out, nil
}
func (s *domainStub) Inspect(ctx context.Context, id sandbox.SandboxID) (sandbox.Detail, error) {
	x, err := s.dto.Inspect(ctx, string(id))
	return sandbox.Detail{Summary: sandbox.Summary{ID: sandbox.SandboxID(x.ID), Name: x.Name, Image: sandbox.ImageID(x.Image), Status: x.Status, State: x.Status, Ports: parseDomainPorts(x.Ports), ExpiresAt: x.ExpiresAt, URL: x.URL}, Running: x.Running, Resources: sandbox.ResourceLimits{MemoryMB: x.Resources.Memory, CPUs: x.Resources.CPUs}, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
}
func (s *domainStub) Start(ctx context.Context, id sandbox.SandboxID) (sandbox.Started, error) {
	x, err := s.dto.Start(ctx, string(id))
	return sandbox.Started{Status: x.Status, Ports: parseDomainPorts(x.Ports), ExpiresAt: x.ExpiresAt}, err
}
func (s *domainStub) Stop(ctx context.Context, id sandbox.SandboxID) error {
	return s.dto.Stop(ctx, string(id))
}
func (s *domainStub) Restart(ctx context.Context, id sandbox.SandboxID) (sandbox.Started, error) {
	x, err := s.dto.Restart(ctx, string(id))
	return sandbox.Started{Status: x.Status, Ports: parseDomainPorts(x.Ports), ExpiresAt: x.ExpiresAt}, err
}
func (s *domainStub) GetNetwork(ctx context.Context, id sandbox.SandboxID) (sandbox.Network, error) {
	x, err := s.dto.GetNetwork(ctx, string(id))
	if err != nil {
		return sandbox.Network{}, err
	}
	main, err := sandbox.ParsePort(x.MainPort)
	if err != nil {
		return sandbox.Network{}, err
	}
	out := sandbox.Network{Main: main}
	for guest, host := range x.PortsMap {
		p, e := sandbox.ParsePort(guest)
		if e != nil {
			return sandbox.Network{}, e
		}
		var n uint16
		if _, e = fmt.Sscanf(host, "%d", &n); e != nil {
			return sandbox.Network{}, e
		}
		out.Ports = append(out.Ports, sandbox.PublishedPort{Guest: p, Host: n})
	}
	return out, nil
}
func (s *domainStub) Remove(ctx context.Context, id sandbox.SandboxID) error {
	return s.dto.Remove(ctx, string(id))
}
func (s *domainStub) Pause(ctx context.Context, id sandbox.SandboxID) error {
	return s.dto.Pause(ctx, string(id))
}
func (s *domainStub) Resume(ctx context.Context, id sandbox.SandboxID) error {
	return s.dto.Resume(ctx, string(id))
}
func (s *domainStub) RenewExpiration(ctx context.Context, id sandbox.SandboxID, d time.Duration) error {
	return s.dto.RenewExpiration(ctx, string(id), int(d/time.Second))
}
func (s *domainStub) Stats(ctx context.Context, id sandbox.SandboxID) (sandbox.Stats, error) {
	x, err := s.dto.Stats(ctx, string(id))
	return sandbox.Stats{CPU: x.CPU, Memory: sandbox.MemoryUsage{Usage: x.Memory.Usage, Limit: x.Memory.Limit, Percent: x.Memory.Percent}, PIDs: x.PIDs}, err
}
func (s *domainStub) ExecCommand(ctx context.Context, id sandbox.SandboxID, r sandbox.ProcessRequest) (sandbox.Command, error) {
	x, err := s.dto.ExecCommand(ctx, string(id), models.ExecCommandRequest{Command: r.Command, Args: r.Args, Cwd: r.Cwd, Env: r.Env})
	return sandbox.Command{ID: sandbox.CommandID(x.ID), SandboxID: sandbox.SandboxID(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
}
func (s *domainStub) GetCommand(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Command, error) {
	x, err := s.dto.GetCommand(ctx, string(id), string(cmd))
	return sandbox.Command{ID: sandbox.CommandID(x.ID), SandboxID: sandbox.SandboxID(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
}
func (s *domainStub) ListCommands(ctx context.Context, id sandbox.SandboxID) ([]sandbox.Command, error) {
	xs, err := s.dto.ListCommands(ctx, string(id))
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.Command, 0, len(xs))
	for _, x := range xs {
		out = append(out, sandbox.Command{ID: sandbox.CommandID(x.ID), SandboxID: sandbox.SandboxID(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt})
	}
	return out, nil
}
func (s *domainStub) KillCommand(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID, signal int) (sandbox.Command, error) {
	x, err := s.dto.KillCommand(ctx, string(id), string(cmd), signal)
	return sandbox.Command{ID: sandbox.CommandID(x.ID), SandboxID: sandbox.SandboxID(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
}
func (s *domainStub) WaitCommand(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Command, error) {
	if s.wait != nil {
		return s.wait(ctx, id, cmd)
	}
	x, err := s.dto.WaitCommand(ctx, string(id), string(cmd))
	return sandbox.Command{ID: sandbox.CommandID(x.ID), SandboxID: sandbox.SandboxID(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
}
func (s *domainStub) StreamCommandLogs(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (io.ReadCloser, io.ReadCloser, error) {
	return s.dto.StreamCommandLogs(ctx, string(id), string(cmd))
}
func (s *domainStub) GetCommandLogs(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Logs, error) {
	x, err := s.dto.GetCommandLogs(ctx, string(id), string(cmd))
	return sandbox.Logs{Stdout: x.Stdout, Stderr: x.Stderr, ExitCode: x.ExitCode}, err
}
func (s *domainStub) ReadFile(ctx context.Context, id sandbox.SandboxID, path string) (string, error) {
	return s.dto.ReadFile(ctx, string(id), path)
}
func (s *domainStub) WriteFile(ctx context.Context, id sandbox.SandboxID, path, content string) error {
	return s.dto.WriteFile(ctx, string(id), path, content)
}
func (s *domainStub) DeleteFile(ctx context.Context, id sandbox.SandboxID, path string) error {
	return s.dto.DeleteFile(ctx, string(id), path)
}
func (s *domainStub) ListDir(ctx context.Context, id sandbox.SandboxID, path string) (string, error) {
	return s.dto.ListDir(ctx, string(id), path)
}
func (s *domainStub) PullImage(ctx context.Context, ref string) error {
	return s.dto.PullImage(ctx, ref)
}
func (s *domainStub) RemoveImage(ctx context.Context, ref string, force bool) error {
	return s.dto.RemoveImage(ctx, ref, force)
}
func (s *domainStub) InspectImage(ctx context.Context, ref string) (sandbox.ImageDetail, error) {
	x, err := s.dto.InspectImage(ctx, ref)
	return sandbox.ImageDetail{ImageSummary: sandbox.ImageSummary{ID: sandbox.ImageID(x.ID), Tags: x.Tags, Size: x.Size}, Created: x.Created, Architecture: x.Architecture, OS: x.OS}, err
}
func (s *domainStub) ListImages(ctx context.Context) ([]sandbox.ImageSummary, error) {
	xs, err := s.dto.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.ImageSummary, 0, len(xs))
	for _, x := range xs {
		out = append(out, sandbox.ImageSummary{ID: sandbox.ImageID(x.ID), Tags: x.Tags, Size: x.Size})
	}
	return out, nil
}

var _ sandbox.Application = (*domainStub)(nil)

// newRouter builds a Gin engine with all sandbox routes registered for the given client.
func newRouter(fixture any) *gin.Engine {
	r := gin.New()
	var app sandbox.Application
	switch value := fixture.(type) {
	case *stub:
		app = &domainStub{dto: value}
	case *cancelableWaitStub:
		app = &domainStub{dto: value.stub, wait: func(ctx context.Context, id sandbox.SandboxID, cmd sandbox.CommandID) (sandbox.Command, error) {
			x, err := value.WaitCommand(ctx, string(id), string(cmd))
			return sandbox.Command{ID: sandbox.CommandID(x.ID), SandboxID: sandbox.SandboxID(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
		}}
	default:
		panic("unsupported API test fixture")
	}
	h := api.New(app)
	h.RegisterHealthCheck(r)
	h.RegisterRoutes(r.Group("/v1"))
	return r
}

// newAuthRouter builds a Gin engine with API key auth enabled on /v1.
func newAuthRouter(d *stub, key string) *gin.Engine {
	r := gin.New()
	h := api.New(&domainStub{dto: d})
	h.RegisterHealthCheck(r)
	v1 := r.Group("/v1")
	v1.Use(api.APIKeyAuth(key))
	h.RegisterRoutes(v1)
	return r
}

// do fires an HTTP request against the router and returns the recorded response.
// body is JSON-encoded when non-nil.
func do(r *gin.Engine, method, url string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &b)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doWithAuth fires an HTTP request with a Bearer token.
func doWithAuth(r *gin.Engine, method, url string, body any, token string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestListSandboxes(t *testing.T) {
	r := newRouter(&stub{
		list: func() ([]models.SandboxSummary, error) {
			return []models.SandboxSummary{{ID: "abc123", Name: "test", Image: "nginx", Status: "running", State: "running"}}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "abc123")
}

func TestCreateSandbox(t *testing.T) {
	r := newRouter(&stub{
		create: func(req models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
			return models.CreateSandboxResponse{
				ID:    "abc123",
				Name:  "eager-turing",
				URL:   "http://eager-turing.localhost:9090",
				Ports: []string{"3000/tcp"},
			}, nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{"image": "nextjs-docker:latest"})
	assert.Equal(t, 201, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "abc123")
	assert.Contains(t, body, "eager-turing")
	assert.Contains(t, body, "http://eager-turing.localhost:9090")
}

func TestCreateSandbox_MissingImage(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{"ports": []string{"3000"}})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestGetSandbox_NotFound(t *testing.T) {
	r := newRouter(&stub{
		inspect: func(string) (models.SandboxDetail, error) {
			return models.SandboxDetail{}, sandbox.ErrNotFound
		},
	})

	w := do(r, "GET", "/v1/sandboxes/nope", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestGetSandbox_ReturnsDetail(t *testing.T) {
	r := newRouter(&stub{
		inspect: func(id string) (models.SandboxDetail, error) {
			return models.SandboxDetail{
				ID:      id,
				Name:    "my-sandbox",
				Image:   "nginx:latest",
				Status:  "running",
				Running: true,
				Ports:   []string{"80/tcp"},
				Resources: models.ResourceLimits{
					Memory: 1024,
					CPUs:   1.0,
				},
				StartedAt: "2026-02-21T02:00:18Z",
			}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "my-sandbox")
	assert.Contains(t, body, "80/tcp")
	assert.Contains(t, body, "running")
	assert.NotContains(t, body, "32770") // host ports should not be exposed
	// Should NOT contain raw Docker inspect noise
	assert.NotContains(t, body, "HostConfig")
	assert.NotContains(t, body, "GraphDriver")
}

func TestDeleteSandbox(t *testing.T) {
	r := newRouter(&stub{
		remove: func(string) error { return nil },
	})

	w := do(r, "DELETE", "/v1/sandboxes/abc123", nil)
	assert.Equal(t, 204, w.Code)
}

func TestStopSandbox(t *testing.T) {
	r := newRouter(&stub{
		stop: func(string) error { return nil },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/stop", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "stopped")
}

func TestRestartSandbox(t *testing.T) {
	r := newRouter(&stub{
		restart: func(string) (models.RestartResponse, error) {
			return models.RestartResponse{
				Status: "restarted",
				Ports:  []string{"3000/tcp"},
			}, nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/restart", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "restarted")
	assert.Contains(t, body, "3000/tcp")
}

func TestRestartSandbox_NotFound(t *testing.T) {
	r := newRouter(&stub{
		restart: func(string) (models.RestartResponse, error) {
			return models.RestartResponse{}, sandbox.ErrNotFound
		},
	})

	w := do(r, "POST", "/v1/sandboxes/nope/restart", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

// ── Command Tests ───────────────────────────────────────────────────────────

func TestExecCommand_OK(t *testing.T) {
	r := newRouter(&stub{
		execCommand: func(sandboxID string, req models.ExecCommandRequest) (models.CommandDetail, error) {
			return models.CommandDetail{
				ID:        "cmd_abc123",
				Name:      req.Command,
				Args:      req.Args,
				Cwd:       req.Cwd,
				SandboxID: sandboxID,
				StartedAt: 1750344501629,
			}, nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd", map[string]any{
		"command": "npm",
		"args":    []string{"install"},
		"cwd":     "/app",
	})
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "cmd_abc123")
	assert.Contains(t, body, "npm")
	assert.Contains(t, body, `"command"`)
	// exit_code should not be present (omitempty)
	assert.NotContains(t, body, "exit_code")
}

func TestExecCommand_MissingCommand(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd", map[string]any{})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestExecCommand_SandboxNotRunning(t *testing.T) {
	r := newRouter(&stub{
		execCommand: func(string, models.ExecCommandRequest) (models.CommandDetail, error) {
			return models.CommandDetail{}, sandbox.ErrNotRunning
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd", map[string]any{"command": "echo"})
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
}

func TestExecCommand_SandboxNotFound(t *testing.T) {
	r := newRouter(&stub{
		execCommand: func(string, models.ExecCommandRequest) (models.CommandDetail, error) {
			return models.CommandDetail{}, sandbox.ErrNotFound
		},
	})

	w := do(r, "POST", "/v1/sandboxes/nope/cmd", map[string]any{"command": "echo"})
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestListCommands_OK(t *testing.T) {
	r := newRouter(&stub{
		listCommands: func(sandboxID string) ([]models.CommandDetail, error) {
			ec := 0
			return []models.CommandDetail{
				{ID: "cmd_1", Name: "echo", SandboxID: sandboxID, ExitCode: &ec, StartedAt: 1000},
				{ID: "cmd_2", Name: "npm", SandboxID: sandboxID, StartedAt: 2000},
			}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "cmd_1")
	assert.Contains(t, body, "cmd_2")
	assert.Contains(t, body, `"commands"`)
}

func TestListCommands_Empty(t *testing.T) {
	r := newRouter(&stub{
		listCommands: func(string) ([]models.CommandDetail, error) {
			return []models.CommandDetail{}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"commands":[]`)
}

func TestGetCommand_OK(t *testing.T) {
	ec := 0
	r := newRouter(&stub{
		getCommand: func(sandboxID, cmdID string) (models.CommandDetail, error) {
			return models.CommandDetail{
				ID:        cmdID,
				Name:      "echo",
				SandboxID: sandboxID,
				ExitCode:  &ec,
				StartedAt: 1000,
			}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd/cmd_xyz", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "cmd_xyz")
	assert.Contains(t, body, `"exit_code"`)
}

func TestGetCommand_NotFound(t *testing.T) {
	r := newRouter(&stub{
		getCommand: func(string, string) (models.CommandDetail, error) {
			return models.CommandDetail{}, sandbox.ErrCommandNotFound
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd/nope", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestKillCommand_OK(t *testing.T) {
	ec := 137
	r := newRouter(&stub{
		killCommand: func(sandboxID, cmdID string, signal int) (models.CommandDetail, error) {
			assert.Equal(t, 15, signal)
			return models.CommandDetail{
				ID:        cmdID,
				Name:      "sleep",
				SandboxID: sandboxID,
				ExitCode:  &ec,
				StartedAt: 1000,
			}, nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd/cmd_xyz/kill", map[string]any{"signal": 15})
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "cmd_xyz")
	assert.Contains(t, body, "137")
}

func TestKillCommand_AlreadyFinished(t *testing.T) {
	r := newRouter(&stub{
		killCommand: func(string, string, int) (models.CommandDetail, error) {
			return models.CommandDetail{}, sandbox.ErrCommandFinished
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd/cmd_xyz/kill", map[string]any{"signal": 9})
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
}

func TestKillCommand_NotFound(t *testing.T) {
	r := newRouter(&stub{
		killCommand: func(string, string, int) (models.CommandDetail, error) {
			return models.CommandDetail{}, sandbox.ErrCommandNotFound
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd/nope/kill", map[string]any{"signal": 9})
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestKillCommand_MissingSignal(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes/abc123/cmd/cmd_xyz/kill", map[string]any{})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

// ── Command Logs Tests ──────────────────────────────────────────────────────

func TestGetCommandLogs_Snapshot(t *testing.T) {
	ec := 0
	r := newRouter(&stub{
		getCommandLogs: func(sandboxID, cmdID string) (models.CommandLogsResponse, error) {
			assert.Equal(t, "abc123", sandboxID)
			assert.Equal(t, "cmd_xyz", cmdID)
			return models.CommandLogsResponse{
				Stdout:   "hello world\n",
				Stderr:   "some warning\n",
				ExitCode: &ec,
			}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd/cmd_xyz/logs", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "hello world")
	assert.Contains(t, w.Body.String(), "some warning")
	assert.Contains(t, w.Body.String(), `"exit_code":0`)
}

func TestGetCommandLogs_NotFound(t *testing.T) {
	r := newRouter(&stub{
		getCommandLogs: func(sandboxID, cmdID string) (models.CommandLogsResponse, error) {
			return models.CommandLogsResponse{}, sandbox.ErrCommandNotFound
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd/cmd_xyz/logs", nil)
	assert.Equal(t, 404, w.Code)
}

func TestGetCommandLogs_StreamMode(t *testing.T) {
	r := newRouter(&stub{
		streamCommandLogs: func(sandboxID, cmdID string) (io.ReadCloser, io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte("line1\n"))),
				io.NopCloser(bytes.NewReader([]byte("err1\n"))),
				nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/cmd/cmd_xyz/logs?stream=true", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/x-ndjson")
	assert.Contains(t, w.Body.String(), "stdout")
}

// ── File Tests ──────────────────────────────────────────────────────────────

func TestReadFile(t *testing.T) {
	r := newRouter(&stub{
		readFile: func(id, path string) (string, error) {
			return "hello world", nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/files?path=/app/page.tsx", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "hello world")
}

func TestReadFile_MissingPath(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "GET", "/v1/sandboxes/abc123/files", nil)
	assert.Equal(t, 400, w.Code)
}

func TestWriteFile(t *testing.T) {
	r := newRouter(&stub{
		writeFile: func(id, path, content string) error { return nil },
	})

	w := do(r, "PUT", "/v1/sandboxes/abc123/files?path=/app/page.tsx", map[string]any{"content": "hello"})
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "written")
}

func TestDeleteFile(t *testing.T) {
	r := newRouter(&stub{
		deleteFile: func(id, path string) error { return nil },
	})

	w := do(r, "DELETE", "/v1/sandboxes/abc123/files?path=/app/page.tsx", nil)
	assert.Equal(t, 204, w.Code)
}

func TestListDir(t *testing.T) {
	r := newRouter(&stub{
		listDir: func(id, path string) (string, error) {
			return "page.tsx\nlayout.tsx\n", nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/files/list?path=/app/src", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "page.tsx")
}

func TestInternalError(t *testing.T) {
	r := newRouter(&stub{
		list: func() ([]models.SandboxSummary, error) {
			return nil, errors.New("daemon unreachable")
		},
	})

	w := do(r, "GET", "/v1/sandboxes", nil)
	assert.Equal(t, 500, w.Code)
	assert.Contains(t, w.Body.String(), "INTERNAL_ERROR")
}

func TestCreateSandbox_WithResourcesAndTimeout(t *testing.T) {
	var captured models.CreateSandboxRequest
	r := newRouter(&stub{
		create: func(req models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
			captured = req
			return models.CreateSandboxResponse{ID: "abc123", Ports: []string{"3000/tcp"}}, nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":   "nextjs-docker:latest",
		"timeout": 3600,
		"resources": map[string]any{
			"memory": 512,
			"cpus":   1.5,
		},
	})
	assert.Equal(t, 201, w.Code)
	assert.Equal(t, 3600, captured.Timeout)
	assert.NotNil(t, captured.Resources)
	assert.Equal(t, int64(512), captured.Resources.Memory)
	assert.Equal(t, 1.5, captured.Resources.CPUs)
}

func TestCreateSandbox_NegativeTimeout(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":   "nextjs-docker:latest",
		"timeout": -1,
	})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestCreateSandbox_NegativeMemory(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":     "nextjs-docker:latest",
		"resources": map[string]any{"memory": -1, "cpus": 1.0},
	})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestCreateSandbox_NegativeCPUs(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":     "nextjs-docker:latest",
		"resources": map[string]any{"memory": 512, "cpus": -0.5},
	})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestCreateSandbox_ExceedsMaxMemory(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":     "nextjs-docker:latest",
		"resources": map[string]any{"memory": 9000, "cpus": 1.0},
	})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
	assert.Contains(t, w.Body.String(), "8192")
}

func TestCreateSandbox_ExceedsMaxCPUs(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image":     "nextjs-docker:latest",
		"resources": map[string]any{"memory": 1024, "cpus": 5.0},
	})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
	assert.Contains(t, w.Body.String(), "4.0")
}

func TestCreateSandbox_DefaultResources(t *testing.T) {
	var captured models.CreateSandboxRequest
	r := newRouter(&stub{
		create: func(req models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
			captured = req
			return models.CreateSandboxResponse{ID: "test123"}, nil
		},
	})

	// Create without specifying resources
	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image": "nextjs-docker:latest",
	})
	assert.Equal(t, 201, w.Code)
	// Resources should be nil in the request (defaults applied in Docker client)
	assert.Nil(t, captured.Resources)
}

func TestPauseSandbox(t *testing.T) {
	r := newRouter(&stub{
		pause: func(string) error { return nil },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/pause", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "paused")
}

func TestPauseSandbox_NotFound(t *testing.T) {
	r := newRouter(&stub{
		pause: func(string) error { return sandbox.ErrNotFound },
	})

	w := do(r, "POST", "/v1/sandboxes/nope/pause", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestResumeSandbox(t *testing.T) {
	r := newRouter(&stub{
		resume: func(string) error { return nil },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/resume", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "resumed")
}

func TestResumeSandbox_NotFound(t *testing.T) {
	r := newRouter(&stub{
		resume: func(string) error { return sandbox.ErrNotFound },
	})

	w := do(r, "POST", "/v1/sandboxes/nope/resume", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestRenewExpiration(t *testing.T) {
	var capturedID string
	var capturedTimeout int
	r := newRouter(&stub{
		renewExpiration: func(id string, timeout int) error {
			capturedID = id
			capturedTimeout = timeout
			return nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/renew-expiration", map[string]any{"timeout": 7200})
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "renewed")
	assert.Contains(t, w.Body.String(), "7200")
	assert.Equal(t, "abc123", capturedID)
	assert.Equal(t, 7200, capturedTimeout)
}

func TestRenewExpiration_NotFound(t *testing.T) {
	r := newRouter(&stub{
		renewExpiration: func(string, int) error { return sandbox.ErrNotFound },
	})

	w := do(r, "POST", "/v1/sandboxes/nope/renew-expiration", map[string]any{"timeout": 3600})
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestRenewExpiration_MissingTimeout(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes/abc123/renew-expiration", map[string]any{})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestRenewExpiration_NegativeTimeout(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes/abc123/renew-expiration", map[string]any{"timeout": -1})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestRenewExpiration_ZeroTimeout(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/sandboxes/abc123/renew-expiration", map[string]any{"timeout": 0})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestGetSandboxNetwork(t *testing.T) {
	r := newRouter(&stub{
		getNetwork: func(id string) (models.SandboxNetwork, error) {
			assert.Equal(t, "abc123", id)
			return models.SandboxNetwork{
				MainPort: "3000/tcp",
				PortsMap: map[string]string{"3000/tcp": "32768", "5173/tcp": "32769"},
			}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/network", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "3000/tcp")
	assert.Contains(t, w.Body.String(), "32769")
}

// ── API Key Auth Tests ──────────────────────────────────────────────────────

func TestApiKeyAuth_NoHeader(t *testing.T) {
	r := newAuthRouter(&stub{
		list: func() ([]models.SandboxSummary, error) {
			return []models.SandboxSummary{}, nil
		},
	}, "sk-test-123")

	w := do(r, "GET", "/v1/sandboxes", nil)
	assert.Equal(t, 401, w.Code)
	assert.Contains(t, w.Body.String(), "UNAUTHORIZED")
}

func TestApiKeyAuth_WrongKey(t *testing.T) {
	r := newAuthRouter(&stub{
		list: func() ([]models.SandboxSummary, error) {
			return []models.SandboxSummary{}, nil
		},
	}, "sk-test-123")

	w := doWithAuth(r, "GET", "/v1/sandboxes", nil, "sk-wrong")
	assert.Equal(t, 401, w.Code)
	assert.Contains(t, w.Body.String(), "UNAUTHORIZED")
}

func TestApiKeyAuth_CorrectKey(t *testing.T) {
	r := newAuthRouter(&stub{
		list: func() ([]models.SandboxSummary, error) {
			return []models.SandboxSummary{{ID: "abc123"}}, nil
		},
	}, "sk-test-123")

	w := doWithAuth(r, "GET", "/v1/sandboxes", nil, "sk-test-123")
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "abc123")
}

func TestNoAuth_WorksWithoutMiddleware(t *testing.T) {
	r := newRouter(&stub{
		list: func() ([]models.SandboxSummary, error) {
			return []models.SandboxSummary{{ID: "abc123"}}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "abc123")
}

// ── Health Check Tests ──────────────────────────────────────────────────────

func TestHealthCheck_Healthy(t *testing.T) {
	r := newRouter(&stub{
		ping: func() error { return nil },
	})

	w := do(r, "GET", "/v1/health", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "healthy")
}

func TestHealthCheck_Unhealthy(t *testing.T) {
	r := newRouter(&stub{
		ping: func() error { return errors.New("daemon unreachable") },
	})

	w := do(r, "GET", "/v1/health", nil)
	assert.Equal(t, 503, w.Code)
	assert.Contains(t, w.Body.String(), "unhealthy")
	assert.Contains(t, w.Body.String(), "daemon unreachable")
}

func TestHealthCheck_NoAuthRequired(t *testing.T) {
	r := newAuthRouter(&stub{
		ping: func() error { return nil },
	}, "sk-test-123")

	// Health check should work without auth header.
	w := do(r, "GET", "/v1/health", nil)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "healthy")
}

func TestPullImage(t *testing.T) {
	var capturedImage string
	r := newRouter(&stub{
		pullImage: func(image string) error {
			capturedImage = image
			return nil
		},
	})

	w := do(r, "POST", "/v1/images/pull", map[string]any{
		"image": "nginx:latest",
	})
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "pulled")
	assert.Contains(t, w.Body.String(), "nginx:latest")
	assert.Equal(t, "nginx:latest", capturedImage)
}

func TestPullImage_MissingImage(t *testing.T) {
	r := newRouter(&stub{})

	w := do(r, "POST", "/v1/images/pull", map[string]any{})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
}

func TestPullImage_Error(t *testing.T) {
	r := newRouter(&stub{
		pullImage: func(string) error {
			return errors.New("registry unreachable")
		},
	})

	w := do(r, "POST", "/v1/images/pull", map[string]any{
		"image": "nginx:latest",
	})
	assert.Equal(t, 500, w.Code)
	assert.Contains(t, w.Body.String(), "INTERNAL_ERROR")
	assert.Contains(t, w.Body.String(), "registry unreachable")
}

func TestCreateSandbox_ImageNotFound(t *testing.T) {
	r := newRouter(&stub{
		create: func(models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
			return models.CreateSandboxResponse{}, sandbox.ErrImageNotFound
		},
	})

	w := do(r, "POST", "/v1/sandboxes", map[string]any{
		"image": "nonexistent:latest",
	})
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "BAD_REQUEST")
	assert.Contains(t, w.Body.String(), "image not found locally")
	assert.Contains(t, w.Body.String(), "/v1/images/pull")
}

// ── Stats Tests ─────────────────────────────────────────────────────────────

func TestGetStats_OK(t *testing.T) {
	r := newRouter(&stub{
		stats: func(id string) (models.SandboxStats, error) {
			return models.SandboxStats{
				CPU: 25.5,
				Memory: models.MemoryUsage{
					Usage:   512 * 1024 * 1024,
					Limit:   1024 * 1024 * 1024,
					Percent: 50.0,
				},
				PIDs: 12,
			}, nil
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/stats", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "25.5")
	assert.Contains(t, body, "50")
	assert.Contains(t, body, `"pids":12`)
}

func TestGetStats_NotFound(t *testing.T) {
	r := newRouter(&stub{
		stats: func(string) (models.SandboxStats, error) {
			return models.SandboxStats{}, sandbox.ErrNotFound
		},
	})

	w := do(r, "GET", "/v1/sandboxes/nope/stats", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

func TestGetStats_Error(t *testing.T) {
	r := newRouter(&stub{
		stats: func(string) (models.SandboxStats, error) {
			return models.SandboxStats{}, errors.New("daemon error")
		},
	})

	w := do(r, "GET", "/v1/sandboxes/abc123/stats", nil)
	assert.Equal(t, 500, w.Code)
	assert.Contains(t, w.Body.String(), "INTERNAL_ERROR")
}

// ── Start Tests ─────────────────────────────────────────────────────────────

func TestStartSandbox(t *testing.T) {
	r := newRouter(&stub{
		start: func(id string) (models.RestartResponse, error) {
			return models.RestartResponse{
				Status: "started",
				Ports:  []string{"3000/tcp"},
			}, nil
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/start", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "started")
	assert.Contains(t, body, "3000/tcp")
}

func TestStartSandbox_NotFound(t *testing.T) {
	r := newRouter(&stub{
		start: func(string) (models.RestartResponse, error) {
			return models.RestartResponse{}, sandbox.ErrNotFound
		},
	})

	w := do(r, "POST", "/v1/sandboxes/nope/start", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

// ── Delete Image Tests ──────────────────────────────────────────────────────

func TestDeleteImage(t *testing.T) {
	var capturedID string
	var capturedForce bool
	r := newRouter(&stub{
		removeImage: func(id string, force bool) error {
			capturedID = id
			capturedForce = force
			return nil
		},
	})

	w := do(r, "DELETE", "/v1/images/nginx:latest", nil)
	assert.Equal(t, 204, w.Code)
	assert.Equal(t, "nginx:latest", capturedID)
	assert.False(t, capturedForce)
}

func TestDeleteImage_Force(t *testing.T) {
	var capturedForce bool
	r := newRouter(&stub{
		removeImage: func(_ string, force bool) error {
			capturedForce = force
			return nil
		},
	})

	w := do(r, "DELETE", "/v1/images/nginx:latest?force=true", nil)
	assert.Equal(t, 204, w.Code)
	assert.True(t, capturedForce)
}

func TestDeleteImage_NotFound(t *testing.T) {
	r := newRouter(&stub{
		removeImage: func(string, bool) error {
			return sandbox.ErrNotFound
		},
	})

	w := do(r, "DELETE", "/v1/images/nope", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

// ── Inspect Image Tests ─────────────────────────────────────────────────────

func TestGetImage(t *testing.T) {
	r := newRouter(&stub{
		inspectImage: func(id string) (models.ImageDetail, error) {
			return models.ImageDetail{
				ID:           "sha256:abc123",
				Tags:         []string{"nginx:latest"},
				Size:         142000000,
				Created:      "2026-01-15T10:00:00Z",
				Architecture: "amd64",
				OS:           "linux",
			}, nil
		},
	})

	w := do(r, "GET", "/v1/images/nginx:latest", nil)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "sha256:abc123")
	assert.Contains(t, body, "amd64")
	assert.Contains(t, body, "linux")
}

func TestGetImage_NotFound(t *testing.T) {
	r := newRouter(&stub{
		inspectImage: func(string) (models.ImageDetail, error) {
			return models.ImageDetail{}, sandbox.ErrNotFound
		},
	})

	w := do(r, "GET", "/v1/images/nope", nil)
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NOT_FOUND")
}

// ── Conflict (409) Tests ────────────────────────────────────────────────────

func TestStartSandbox_AlreadyRunning(t *testing.T) {
	r := newRouter(&stub{
		start: func(string) (models.RestartResponse, error) {
			return models.RestartResponse{}, sandbox.ErrAlreadyRunning
		},
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/start", nil)
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
	assert.Contains(t, w.Body.String(), "already running")
}

func TestStopSandbox_AlreadyStopped(t *testing.T) {
	r := newRouter(&stub{
		stop: func(string) error { return sandbox.ErrAlreadyStopped },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/stop", nil)
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
	assert.Contains(t, w.Body.String(), "already stopped")
}

func TestPauseSandbox_AlreadyPaused(t *testing.T) {
	r := newRouter(&stub{
		pause: func(string) error { return sandbox.ErrAlreadyPaused },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/pause", nil)
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
	assert.Contains(t, w.Body.String(), "already paused")
}

func TestPauseSandbox_NotRunning(t *testing.T) {
	r := newRouter(&stub{
		pause: func(string) error { return sandbox.ErrNotRunning },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/pause", nil)
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
	assert.Contains(t, w.Body.String(), "not running")
}

func TestResumeSandbox_NotPaused(t *testing.T) {
	r := newRouter(&stub{
		resume: func(string) error { return sandbox.ErrNotPaused },
	})

	w := do(r, "POST", "/v1/sandboxes/abc123/resume", nil)
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "CONFLICT")
	assert.Contains(t, w.Body.String(), "not paused")
}

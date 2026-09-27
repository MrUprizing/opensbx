package api

import (
	"context"
	"fmt"
	"io"
	"math"
	"opensbx/internal/sandbox"
	"opensbx/models"
	"strconv"
	"time"
)

// facade is the REST/MCP anti-corruption boundary. JSON DTOs never enter the
// application/runtime contracts, and opaque native handles never enter DTOs.
type facade struct{ app sandbox.Application }

func (f *facade) Ping(ctx context.Context) error { return f.app.Ping(ctx) }
func duration(seconds int) (time.Duration, error) {
	if seconds < 0 || int64(seconds) > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("%w: invalid timeout", sandbox.ErrInvalidInput)
	}
	return time.Duration(seconds) * time.Second, nil
}
func (f *facade) Create(ctx context.Context, req models.CreateSandboxRequest) (models.CreateSandboxResponse, error) {
	ports, err := sandbox.ParsePorts(req.Ports)
	if err != nil {
		return models.CreateSandboxResponse{}, fmt.Errorf("%w: %v", sandbox.ErrInvalidInput, err)
	}
	timeout, err := duration(req.Timeout)
	if err != nil {
		return models.CreateSandboxResponse{}, err
	}
	resources := sandbox.ResourceLimits{}
	if req.Resources != nil {
		resources = sandbox.ResourceLimits{MemoryMB: req.Resources.Memory, CPUs: req.Resources.CPUs}
	}
	x, err := f.app.Create(ctx, sandbox.CreateOptions{Image: req.Image, Ports: ports, Timeout: timeout, Resources: resources, Env: req.Env})
	return models.CreateSandboxResponse{ID: string(x.ID), Name: x.Name, Ports: sandbox.PortStrings(x.Ports), URL: x.URL}, err
}
func summary(x sandbox.Summary) models.SandboxSummary {
	return models.SandboxSummary{ID: string(x.ID), Name: x.Name, Image: string(x.Image), Status: x.Status, State: x.State, Ports: sandbox.PortStrings(x.Ports), ExpiresAt: x.ExpiresAt, URL: x.URL}
}
func (f *facade) List(ctx context.Context) ([]models.SandboxSummary, error) {
	items, err := f.app.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.SandboxSummary, 0, len(items))
	for _, item := range items {
		out = append(out, summary(item))
	}
	return out, nil
}
func (f *facade) Inspect(ctx context.Context, id string) (models.SandboxDetail, error) {
	x, err := f.app.Inspect(ctx, sandbox.SandboxID(id))
	return models.SandboxDetail{ID: string(x.ID), Name: x.Name, Image: string(x.Image), Status: x.Status, Running: x.Running, Ports: sandbox.PortStrings(x.Ports), Resources: models.ResourceLimits{Memory: x.Resources.MemoryMB, CPUs: x.Resources.CPUs}, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, ExpiresAt: x.ExpiresAt, URL: x.URL}, err
}
func restart(x sandbox.Started, err error) (models.RestartResponse, error) {
	return models.RestartResponse{Status: x.Status, Ports: sandbox.PortStrings(x.Ports), ExpiresAt: x.ExpiresAt}, err
}
func (f *facade) Start(ctx context.Context, id string) (models.RestartResponse, error) {
	return restart(f.app.Start(ctx, sandbox.SandboxID(id)))
}
func (f *facade) Restart(ctx context.Context, id string) (models.RestartResponse, error) {
	return restart(f.app.Restart(ctx, sandbox.SandboxID(id)))
}
func (f *facade) Stop(ctx context.Context, id string) error {
	return f.app.Stop(ctx, sandbox.SandboxID(id))
}
func (f *facade) Remove(ctx context.Context, id string) error {
	return f.app.Remove(ctx, sandbox.SandboxID(id))
}
func (f *facade) Pause(ctx context.Context, id string) error {
	return f.app.Pause(ctx, sandbox.SandboxID(id))
}
func (f *facade) Resume(ctx context.Context, id string) error {
	return f.app.Resume(ctx, sandbox.SandboxID(id))
}
func (f *facade) RenewExpiration(ctx context.Context, id string, seconds int) error {
	d, err := duration(seconds)
	if err != nil {
		return err
	}
	return f.app.RenewExpiration(ctx, sandbox.SandboxID(id), d)
}
func (f *facade) GetNetwork(ctx context.Context, id string) (models.SandboxNetwork, error) {
	x, err := f.app.GetNetwork(ctx, sandbox.SandboxID(id))
	out := models.SandboxNetwork{MainPort: x.Main.String(), PortsMap: map[string]string{}}
	for _, p := range x.Ports {
		out.PortsMap[p.Guest.String()] = strconv.Itoa(int(p.Host))
	}
	return out, err
}
func (f *facade) Stats(ctx context.Context, id string) (models.SandboxStats, error) {
	x, err := f.app.Stats(ctx, sandbox.SandboxID(id))
	return models.SandboxStats{CPU: x.CPU, Memory: models.MemoryUsage{Usage: x.Memory.Usage, Limit: x.Memory.Limit, Percent: x.Memory.Percent}, PIDs: x.PIDs}, err
}
func commandDTO(x sandbox.Command, err error) (models.CommandDetail, error) {
	return models.CommandDetail{ID: string(x.ID), SandboxID: string(x.SandboxID), Name: x.Name, Args: x.Args, Cwd: x.Cwd, ExitCode: x.ExitCode, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt}, err
}
func (f *facade) ExecCommand(ctx context.Context, id string, req models.ExecCommandRequest) (models.CommandDetail, error) {
	return commandDTO(f.app.ExecCommand(ctx, sandbox.SandboxID(id), sandbox.ProcessRequest{Command: req.Command, Args: req.Args, Cwd: req.Cwd, Env: req.Env}))
}
func (f *facade) GetCommand(ctx context.Context, id, cmd string) (models.CommandDetail, error) {
	return commandDTO(f.app.GetCommand(ctx, sandbox.SandboxID(id), sandbox.CommandID(cmd)))
}
func (f *facade) ListCommands(ctx context.Context, id string) ([]models.CommandDetail, error) {
	items, err := f.app.ListCommands(ctx, sandbox.SandboxID(id))
	if err != nil {
		return nil, err
	}
	out := make([]models.CommandDetail, 0, len(items))
	for _, item := range items {
		x, _ := commandDTO(item, nil)
		out = append(out, x)
	}
	return out, nil
}
func (f *facade) KillCommand(ctx context.Context, id, cmd string, signal int) (models.CommandDetail, error) {
	return commandDTO(f.app.KillCommand(ctx, sandbox.SandboxID(id), sandbox.CommandID(cmd), signal))
}
func (f *facade) WaitCommand(ctx context.Context, id, cmd string) (models.CommandDetail, error) {
	return commandDTO(f.app.WaitCommand(ctx, sandbox.SandboxID(id), sandbox.CommandID(cmd)))
}
func (f *facade) GetCommandLogs(ctx context.Context, id, cmd string) (models.CommandLogsResponse, error) {
	x, err := f.app.GetCommandLogs(ctx, sandbox.SandboxID(id), sandbox.CommandID(cmd))
	return models.CommandLogsResponse{Stdout: x.Stdout, Stderr: x.Stderr, ExitCode: x.ExitCode}, err
}
func (f *facade) StreamCommandLogs(ctx context.Context, id, cmd string) (io.ReadCloser, io.ReadCloser, error) {
	return f.app.StreamCommandLogs(ctx, sandbox.SandboxID(id), sandbox.CommandID(cmd))
}
func (f *facade) ReadFile(ctx context.Context, id, path string) (string, error) {
	return f.app.ReadFile(ctx, sandbox.SandboxID(id), path)
}
func (f *facade) WriteFile(ctx context.Context, id, path, content string) error {
	return f.app.WriteFile(ctx, sandbox.SandboxID(id), path, content)
}
func (f *facade) DeleteFile(ctx context.Context, id, path string) error {
	return f.app.DeleteFile(ctx, sandbox.SandboxID(id), path)
}
func (f *facade) ListDir(ctx context.Context, id, path string) (string, error) {
	return f.app.ListDir(ctx, sandbox.SandboxID(id), path)
}
func (f *facade) PullImage(ctx context.Context, ref string) error { return f.app.PullImage(ctx, ref) }
func (f *facade) RemoveImage(ctx context.Context, ref string, force bool) error {
	return f.app.RemoveImage(ctx, ref, force)
}
func (f *facade) ListImages(ctx context.Context) ([]models.ImageSummary, error) {
	items, err := f.app.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.ImageSummary, 0, len(items))
	for _, x := range items {
		out = append(out, models.ImageSummary{ID: string(x.ID), Tags: x.Tags, Size: x.Size})
	}
	return out, nil
}
func (f *facade) InspectImage(ctx context.Context, ref string) (models.ImageDetail, error) {
	x, err := f.app.InspectImage(ctx, ref)
	return models.ImageDetail{ID: string(x.ID), Tags: x.Tags, Size: x.Size, Created: x.Created, Architecture: x.Architecture, OS: x.OS}, err
}

package runtimeio

import (
	"context"
	"errors"
	"fmt"
	"opensbx/internal/database"
	"opensbx/internal/sandbox"
	"strconv"
	"time"
)

type Adapter struct {
	engine Engine
	cache  NativeCache
	repo   *database.Repository
}

func New(engine Engine, cache NativeCache, repo *database.Repository) *Adapter {
	return &Adapter{engine, cache, repo.PublicView()}
}
func (a *Adapter) native(id sandbox.SandboxID) (*database.Sandbox, error) {
	row, err := a.repo.FindByID(string(id))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, sandbox.ErrNotFound
	}
	return row, nil
}
func (a *Adapter) Ping(ctx context.Context) error { return a.engine.Ping(ctx) }
func (a *Adapter) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	return a.cache.Capabilities(ctx)
}

type prepared struct {
	owner    *Adapter
	ref      string
	identity sandbox.Provenance
}

func (p *prepared) Identity() sandbox.Provenance { return p.identity }
func (a *Adapter) Materialize(ctx context.Context, image sandbox.Image) (sandbox.PreparedImage, error) {
	if image.Validate != nil {
		if err := image.Validate(ctx); err != nil {
			return nil, err
		}
	}
	ref, err := a.cache.Materialize(ctx, image)
	if err != nil {
		return nil, err
	}
	return &prepared{a, ref, sandbox.Provenance{Root: image.RootDigest, Manifest: image.ManifestDigest}}, nil
}
func (a *Adapter) Create(ctx context.Context, opts sandbox.RunOptions) (sandbox.Provisioned, error) {
	if opts.ID == "" {
		return nil, fmt.Errorf("%w: public sandbox identity is required", sandbox.ErrInvalidInput)
	}
	if row, err := a.repo.FindByID(string(opts.ID)); err != nil {
		return nil, err
	} else if row != nil {
		return nil, fmt.Errorf("sandbox identity already exists")
	}
	image, ok := opts.Image.(*prepared)
	if !ok || image.owner != a {
		return nil, fmt.Errorf("image was not prepared by this adapter")
	}
	ctx = sandbox.WithCreationImage(sandbox.WithCreationID(ctx, opts.ID), image.identity.Root)
	result, err := a.engine.Create(ctx, CreateSandboxRequest{Image: image.ref, Ports: sandbox.PortStrings(opts.Ports), Timeout: int(opts.Timeout / time.Second), Resources: &ResourceLimits{Memory: opts.Resources.MemoryMB, CPUs: opts.Resources.CPUs}, Env: opts.Env})
	if err != nil {
		return nil, err
	}
	name := result.Name
	if name == result.ID {
		name = string(opts.ID)
	}
	ports, err := sandbox.ParsePorts(result.Ports)
	lease := &provisioned{a: a, public: opts.ID, native: result.ID, image: image, created: sandbox.Created{ID: opts.ID, Name: name, Ports: ports}}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rollbackErr := lease.Rollback(cleanup)
		if rollbackErr != nil {
			cause := errors.Join(err, rollbackErr)
			return nil, errors.Join(cause, lease.Recover(cleanup, image.identity, cause))
		}
		return nil, err
	}
	return lease, nil
}
func root(row database.Sandbox) sandbox.ImageID {
	if row.ImageRoot != "" {
		return sandbox.ImageID(row.ImageRoot)
	}
	return sandbox.ImageID(row.Image)
}
func (a *Adapter) List(ctx context.Context) ([]sandbox.Summary, error) {
	items, err := a.engine.List(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := a.repo.FindAll()
	if err != nil {
		return nil, err
	}
	owners := map[string]database.Sandbox{}
	for _, row := range rows {
		owners[row.RuntimeID()] = row
	}
	out := make([]sandbox.Summary, 0, len(items))
	for _, item := range items {
		row, ok := owners[item.ID]
		if !ok {
			return nil, sandbox.ErrNotFound
		}
		ports, err := sandbox.ParsePorts(item.Ports)
		if err != nil {
			return nil, err
		}
		out = append(out, sandbox.Summary{ID: sandbox.SandboxID(row.ID), Name: row.Name, Image: root(row), Status: item.Status, State: item.State, Ports: ports, ExpiresAt: item.ExpiresAt})
	}
	return out, nil
}
func (a *Adapter) Inspect(ctx context.Context, id sandbox.SandboxID) (sandbox.Detail, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Detail{}, err
	}
	item, err := a.engine.Inspect(ctx, row.RuntimeID())
	if err != nil {
		return sandbox.Detail{}, err
	}
	ports, err := sandbox.ParsePorts(item.Ports)
	if err != nil {
		return sandbox.Detail{}, err
	}
	return sandbox.Detail{Summary: sandbox.Summary{ID: id, Name: row.Name, Image: root(*row), Status: item.Status, State: item.Status, Ports: ports, ExpiresAt: item.ExpiresAt}, Running: item.Running, Resources: sandbox.ResourceLimits{MemoryMB: item.Resources.Memory, CPUs: item.Resources.CPUs}, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt}, nil
}
func started(item RestartResponse, err error) (sandbox.Started, error) {
	if err != nil {
		return sandbox.Started{}, err
	}
	ports, err := sandbox.ParsePorts(item.Ports)
	return sandbox.Started{Status: item.Status, Ports: ports, ExpiresAt: item.ExpiresAt}, err
}
func (a *Adapter) Start(ctx context.Context, id sandbox.SandboxID) (sandbox.Started, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Started{}, err
	}
	return started(a.engine.Start(ctx, row.RuntimeID()))
}
func (a *Adapter) Restart(ctx context.Context, id sandbox.SandboxID) (sandbox.Started, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Started{}, err
	}
	return started(a.engine.Restart(ctx, row.RuntimeID()))
}
func (a *Adapter) Stop(ctx context.Context, id sandbox.SandboxID) error {
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.Stop(ctx, row.RuntimeID())
}
func (a *Adapter) Remove(ctx context.Context, id sandbox.SandboxID) error {
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.Remove(ctx, row.RuntimeID())
}
func (a *Adapter) Pause(ctx context.Context, id sandbox.SandboxID) error {
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.Pause(ctx, row.RuntimeID())
}
func (a *Adapter) Resume(ctx context.Context, id sandbox.SandboxID) error {
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.Resume(ctx, row.RuntimeID())
}
func (a *Adapter) RenewExpiration(ctx context.Context, id sandbox.SandboxID, timeout time.Duration) error {
	if timeout <= 0 || timeout%time.Second != 0 {
		return fmt.Errorf("%w: timeout must be positive whole seconds", sandbox.ErrInvalidInput)
	}
	row, err := a.native(id)
	if err != nil {
		return err
	}
	return a.engine.RenewExpiration(ctx, row.RuntimeID(), int(timeout/time.Second))
}
func network(item SandboxNetwork) (sandbox.Network, error) {
	out := sandbox.Network{}
	if item.MainPort != "" {
		p, err := sandbox.ParsePort(item.MainPort)
		if err != nil {
			return out, err
		}
		out.Main = p
	}
	for raw, host := range item.PortsMap {
		p, err := sandbox.ParsePort(raw)
		if err != nil {
			return out, err
		}
		n, err := strconv.ParseUint(host, 10, 16)
		if err != nil || n == 0 {
			return out, fmt.Errorf("invalid published port")
		}
		out.Ports = append(out.Ports, sandbox.PublishedPort{Guest: p, Host: uint16(n)})
	}
	return out, nil
}
func (a *Adapter) GetNetwork(ctx context.Context, id sandbox.SandboxID) (sandbox.Network, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Network{}, err
	}
	item, err := a.engine.GetNetwork(ctx, row.RuntimeID())
	if err != nil {
		return sandbox.Network{}, err
	}
	return network(item)
}
func (a *Adapter) Routing(ctx context.Context, id sandbox.SandboxID) (sandbox.Route, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Route{}, err
	}
	item, err := a.engine.Routing(ctx, row.RuntimeID())
	if err != nil {
		return sandbox.Route{}, err
	}
	n, err := network(item.Network)
	return sandbox.Route{Running: item.Running, Network: n}, err
}
func (a *Adapter) Stats(ctx context.Context, id sandbox.SandboxID) (sandbox.Stats, error) {
	row, err := a.native(id)
	if err != nil {
		return sandbox.Stats{}, err
	}
	item, err := a.engine.Stats(ctx, row.RuntimeID())
	return sandbox.Stats{CPU: item.CPU, Memory: sandbox.MemoryUsage{Usage: item.Memory.Usage, Limit: item.Memory.Limit, Percent: item.Memory.Percent}, PIDs: item.PIDs}, err
}

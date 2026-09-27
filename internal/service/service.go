// Package service orchestrates domain operations; HTTP DTOs and native handles
// are confined to transport and runtime adapters respectively.
package service

import (
	"context"
	"errors"
	"fmt"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"golang.org/x/sync/singleflight"
	"math"
	"net"
	"opensbx/internal/database"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	sandbox.Runtime
	cache  sandbox.Cache
	images *images.Store
	repo   *database.Repository
	caps   sandbox.Capabilities
	port   string
	routes singleflight.Group
}

func New(ctx context.Context, r sandbox.Runtime, cache sandbox.Cache, store *images.Store, repo *database.Repository) (*Service, error) {
	caps, err := cache.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if caps.Platform.OS != "linux" {
		return nil, fmt.Errorf("%w: only native Linux images are supported", sandbox.ErrUnsupported)
	}
	return &Service{Runtime: r, cache: cache, images: store, repo: repo.PublicView(), caps: caps}, nil
}
func (s *Service) platform() v1.Platform {
	return v1.Platform{OS: s.caps.Platform.OS, Architecture: s.caps.Platform.Architecture, Variant: s.caps.Platform.Variant}
}
func (s *Service) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Created, error) {
	if math.IsNaN(opts.Resources.CPUs) || math.IsInf(opts.Resources.CPUs, 0) || opts.Resources.CPUs < 0 || opts.Resources.CPUs > 4 || opts.Resources.MemoryMB < 0 || opts.Resources.MemoryMB > 8192 || opts.Timeout < 0 {
		return sandbox.Created{}, fmt.Errorf("%w: invalid sandbox limits", sandbox.ErrInvalidInput)
	}
	if opts.Timeout%time.Second != 0 {
		return sandbox.Created{}, fmt.Errorf("%w: timeout must use whole seconds", sandbox.ErrInvalidInput)
	}
	for _, port := range opts.Ports {
		if _, err := sandbox.ParsePort(port.String()); err != nil {
			return sandbox.Created{}, fmt.Errorf("%w: %v", sandbox.ErrInvalidInput, err)
		}
	}
	if !s.caps.FractionalCPU && math.Trunc(opts.Resources.CPUs) != opts.Resources.CPUs {
		return sandbox.Created{}, fmt.Errorf("%w: fractional CPUs", sandbox.ErrUnsupported)
	}
	image, err := s.images.ResolveImage(ctx, opts.Image, s.caps.Platform)
	if err != nil {
		return sandbox.Created{}, err
	}
	prepared, err := s.cache.Materialize(ctx, image)
	if err != nil {
		return sandbox.Created{}, err
	}
	if prepared == nil {
		return sandbox.Created{}, errors.New("runtime returned no prepared image")
	}
	identity := sandbox.Provenance{Root: image.RootDigest, Manifest: image.ManifestDigest, CacheVersion: s.caps.Runtime + ":" + s.caps.Version}
	if prepared.Identity().Root != identity.Root || prepared.Identity().Manifest != identity.Manifest {
		return sandbox.Created{}, errors.New("prepared image identity mismatch")
	}
	id, err := sandbox.NewID()
	if err != nil {
		return sandbox.Created{}, err
	}
	created, err := s.Runtime.Create(ctx, sandbox.RunOptions{ID: id, Image: prepared, Ports: opts.Ports, Timeout: opts.Timeout, Resources: opts.Resources, Env: opts.Env})
	if err != nil {
		return sandbox.Created{}, err
	}
	if created == nil {
		return sandbox.Created{}, errors.New("runtime returned no creation transaction")
	}
	result := created.Sandbox()
	err = created.Adopt(ctx, identity)
	if result.ID != id {
		err = errors.Join(err, errors.New("created sandbox public identity mismatch"))
	}
	if err == nil {
		row, readErr := s.repo.FindByID(string(id))
		if readErr != nil {
			err = readErr
		} else if row == nil {
			err = errors.New("created sandbox lacks ownership metadata")
		} else {
			result.Name = row.Name
			result.URL = s.url(*row)
		}
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rollbackErr := created.Rollback(cleanup); rollbackErr != nil {
			cause := errors.Join(err, rollbackErr)
			return sandbox.Created{}, errors.Join(cause, created.Recover(cleanup, identity, cause))
		}
		return sandbox.Created{}, err
	}
	return result, nil
}
func (s *Service) SetAddress(addr net.Addr) { _, s.port, _ = net.SplitHostPort(addr.String()) }
func (s *Service) Route(ctx context.Context, name string) (string, error) {
	row, err := s.repo.FindByName(name)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", sandbox.ErrNotFound
	}
	wait := s.routes.DoChan(row.ID, func() (any, error) {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return s.Runtime.Routing(readCtx, sandbox.SandboxID(row.ID))
	})
	var route sandbox.Route
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-wait:
		if result.Err != nil {
			return "", result.Err
		}
		route = result.Val.(sandbox.Route)
	}
	if !route.Running {
		return "", sandbox.ErrNotRunning
	}
	if route.Network.Main.Protocol != "tcp" {
		return "", errors.New("sandbox main port is not TCP")
	}
	for _, p := range route.Network.Ports {
		if p.Guest == route.Network.Main {
			return strconv.Itoa(int(p.Host)), nil
		}
	}
	return "", nil
}
func (s *Service) url(row database.Sandbox) string {
	if row.Port == "" && len(row.Ports) == 1 {
		for p := range row.Ports {
			row.Port = p
		}
	}
	if s.port == "" || row.Name == "" || !strings.HasSuffix(row.Port, "/tcp") {
		return ""
	}
	n, err := strconv.Atoi(row.Ports[row.Port])
	if err != nil || n < 1 || n > 65535 {
		return ""
	}
	return "http://" + row.Name + ".localhost:" + s.port
}
func (s *Service) List(ctx context.Context) ([]sandbox.Summary, error) {
	items, err := s.Runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.FindAll()
	if err != nil {
		return nil, err
	}
	byID := map[sandbox.SandboxID]database.Sandbox{}
	for _, row := range rows {
		byID[sandbox.SandboxID(row.ID)] = row
	}
	for i := range items {
		row, ok := byID[items[i].ID]
		if !ok {
			return nil, sandbox.ErrNotFound
		}
		items[i].URL = s.url(row)
	}
	return items, nil
}
func (s *Service) Inspect(ctx context.Context, id sandbox.SandboxID) (sandbox.Detail, error) {
	item, err := s.Runtime.Inspect(ctx, id)
	if err != nil {
		return sandbox.Detail{}, err
	}
	row, err := s.repo.FindByID(string(id))
	if err != nil {
		return sandbox.Detail{}, err
	}
	if row == nil {
		return sandbox.Detail{}, sandbox.ErrNotFound
	}
	item.URL = s.url(*row)
	return item, nil
}
func (s *Service) Pause(ctx context.Context, id sandbox.SandboxID) error {
	if !s.caps.Pause {
		return fmt.Errorf("%w: pause", sandbox.ErrUnsupported)
	}
	return s.Runtime.Pause(ctx, id)
}
func (s *Service) Resume(ctx context.Context, id sandbox.SandboxID) error {
	if !s.caps.Pause {
		return fmt.Errorf("%w: resume", sandbox.ErrUnsupported)
	}
	return s.Runtime.Resume(ctx, id)
}
func (s *Service) PullImage(ctx context.Context, ref string) error {
	return s.images.Pull(ctx, ref, s.platform())
}
func (s *Service) RemoveImage(ctx context.Context, ref string, force bool) error {
	return s.images.Remove(ctx, ref, force)
}
func (s *Service) ListImages(ctx context.Context) ([]sandbox.ImageSummary, error) {
	return s.images.List(ctx)
}
func (s *Service) InspectImage(ctx context.Context, ref string) (sandbox.ImageDetail, error) {
	return s.images.Inspect(ctx, ref, s.platform())
}

var _ sandbox.Application = (*Service)(nil)

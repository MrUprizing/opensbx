package docker

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"

	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// Client wraps the Docker SDK and exposes sandbox operations.
type Client struct {
	createAttempts sync.Map // Native ID -> operation held until the service Adopt boundary.
	recovery       runtimeio.RecoveryQueue
	cli            *moby.Client
	repo           *database.Repository
	lifecycleMu    sync.Mutex        // serializes native lifecycle mutations and expiration
	timersMu       sync.Mutex        // protects timer replacement and cancellation
	closing        bool              // guarded by lifecycleMu
	timers         sync.Map          // map[containerID]*timerEntry
	commands       sync.Map          // map[cmdID]*runningCommand
	onCacheInvalid func(name string) // called when a sandbox's ports change or it is removed
}

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
		if err := ValidateEndpoint(cli.DaemonHost()); err != nil {
			panic(err)
		}
		mobyClient = cli
	})
	return &Client{cli: mobyClient, repo: repo.NativeView()}
}

func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "unix":
		if u.Host == "" && filepath.IsAbs(u.Path) {
			return nil
		}
	case "npipe":
		if u.Host == "" && strings.HasPrefix(u.Path, "//./pipe/") {
			return nil
		}
	case "tcp", "http", "https":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if u.User == nil && (host == "localhost" || ip != nil && ip.IsLoopback()) && u.Port() != "" && u.Path == "" {
			return nil
		}
	}
	return fmt.Errorf("Docker endpoint %q is not a local socket or loopback endpoint", raw)
}

// Ping checks connectivity with the Docker daemon.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx, moby.PingOptions{})
	return err
}

func (c *Client) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	info, err := c.cli.Info(ctx, moby.InfoOptions{})
	if err != nil {
		return sandbox.Capabilities{}, err
	}
	arch := info.Info.Architecture
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	return sandbox.Capabilities{Runtime: "docker", Version: info.Info.ServerVersion + "/docker-archive-v2", Platform: sandbox.Platform{OS: info.Info.OSType, Architecture: arch}, Pause: true, FractionalCPU: true}, nil
}

// wrapNotFound converts Docker "not found" errors to ErrNotFound.
func wrapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errdefs.IsNotFound(err) {
		return sandbox.ErrNotFound
	}
	return err
}

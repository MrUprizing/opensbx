package docker

import (
	"context"
	moby "github.com/moby/moby/client"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func (c *Client) Routing(ctx context.Context, id string) (runtimeio.RoutingState, error) {
	row, err := c.repo.FindByID(id)
	if err != nil {
		return runtimeio.RoutingState{}, err
	}
	if row == nil {
		return runtimeio.RoutingState{}, sandbox.ErrNotFound
	}
	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.RoutingState{}, wrapNotFound(err)
	}
	ports := extractPorts(info.Container.NetworkSettings.Ports)
	main := row.Port
	if main == "" && len(ports) == 1 {
		for p := range ports {
			main = p
		}
	}
	return runtimeio.RoutingState{Running: info.Container.State.Running, Network: runtimeio.SandboxNetwork{MainPort: main, PortsMap: ports}}, nil
}

// GetNetwork returns current exposed port mappings and selected main routing port.
func (c *Client) GetNetwork(ctx context.Context, id string) (runtimeio.SandboxNetwork, error) {
	sb, err := c.repo.FindByID(id)
	if err != nil {
		return runtimeio.SandboxNetwork{}, err
	}
	if sb == nil {
		return runtimeio.SandboxNetwork{}, sandbox.ErrNotFound
	}

	info, err := c.cli.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	if err != nil {
		return runtimeio.SandboxNetwork{}, wrapNotFound(err)
	}

	ports := extractPorts(info.Container.NetworkSettings.Ports)
	mainPort := sb.Port
	if mainPort == "" && len(ports) == 1 {
		for p := range ports {
			mainPort = p
		}
	}

	return runtimeio.SandboxNetwork{MainPort: mainPort, PortsMap: ports}, nil
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

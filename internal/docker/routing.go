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
func (c *Client) DiscardCreated(ctx context.Context, id string) error { return c.Remove(ctx, id) }

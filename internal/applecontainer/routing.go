package applecontainer

import (
	"context"
	"opensbx/internal/runtimeio"
)

// Routing reads one coherent inventory entry, not separate inspect/network CLI
// calls for every asset. Ownership and running state remain live checks.
func (c *Client) Routing(ctx context.Context, id string) (runtimeio.RoutingState, error) {
	info, err := c.lookup(ctx, id)
	if err != nil {
		return runtimeio.RoutingState{}, err
	}
	row, err := c.owned(id)
	if err != nil {
		return runtimeio.RoutingState{}, err
	}
	ports, err := networkPorts(info)
	if err != nil {
		return runtimeio.RoutingState{}, err
	}
	return runtimeio.RoutingState{Running: info.Status.State == "running", Network: runtimeio.SandboxNetwork{MainPort: row.Port, PortsMap: ports}}, nil
}
func (c *Client) DiscardCreated(ctx context.Context, id string) error {
	// A returned creation lease already has ownership and a durable intent.
	// Use the same transactional cleanup as explicit removal; if the database
	// is unavailable, retain that intent rather than losing recovery evidence.
	err := c.Remove(ctx, id)
	if err != nil {
		c.CreationFailed(id)
	} else {
		c.createAttempts.Delete(id)
	}
	return err
}

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
	c.mu.Lock()
	defer c.mu.Unlock()
	// Compensation must work even if the ownership DB write/read failed. The
	// creation lease supplies the exact newly generated ID; rollback verifies
	// its live label before deleting anything. Database cleanup belongs to lease.
	if err := c.rollback(id); err != nil {
		return err
	}
	c.clearTimer(id)
	c.changed(id)
	delete(c.finished, id)
	for cmdID, cmd := range c.commands {
		if cmd.sandboxID == id {
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

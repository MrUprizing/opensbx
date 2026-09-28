package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func (c *Client) AdoptCreation(ctx context.Context, row database.Sandbox) error {
	for !c.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closing {
		return errors.New("backend is shutting down")
	}
	err := c.repo.UpdateProvenance(row, true)
	if err == nil {
		c.createAttempts.Delete(row.RuntimeID())
	}
	return err
}

// Recover performs only durable, runtime-scoped recovery before serving traffic.
// Construction and Ping remain read-only with respect to sandbox lifecycle.
func (c *Client) Recover(ctx context.Context) error {
	if err := c.lockRecovery(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if c.closing {
		return errors.New("backend is shutting down")
	}
	repo := c.repo.WithContext(ctx)
	ops, err := repo.Operations("container")
	if err != nil {
		return err
	}
	rows, err := repo.FindAll()
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := repo.ValidateOperation(op); err != nil {
			return err
		}
		if err := validateIntent(op); err != nil {
			return err
		}
		c.retryOperation(op)
	}
	for _, row := range rows {
		if row.RuntimeKind != "container" || row.ExpiresAt == nil {
			continue
		}
		delay := row.ExpiresAt.Sub(c.now())
		if delay <= 0 {
			delay = runtimeio.RecoveryRetryDelay
		}
		c.scheduleDeadline(row.RuntimeID(), *row.ExpiresAt, delay)
	}
	warm, cancel := context.WithTimeout(ctx, runtimeio.RecoveryWarmupBudget)
	defer cancel()
	for _, op := range ops {
		if warm.Err() != nil {
			break
		}
		if err := c.reconcile(warm, op); err != nil {
			if !runtimeio.IsDeferredRecovery(err) && !(warm.Err() != nil && errors.Is(err, warm.Err())) {
				return err
			}
			log.Printf("Apple startup intent %s deferred to background recovery: %v", op.ID, err)
		}
	}
	for _, row := range rows {
		if warm.Err() != nil {
			break
		}
		if row.RuntimeKind != "container" || row.ExpiresAt == nil || row.ExpiresAt.After(c.now()) {
			continue
		}
		pending, err := c.repo.Pending("container", row.RuntimeID())
		if err != nil {
			return err
		}
		if pending != nil {
			continue
		}
		if err := c.stopLocked(warm, row.RuntimeID()); err != nil && !errors.Is(err, sandbox.ErrAlreadyStopped) && !errors.Is(err, sandbox.ErrNotFound) {
			if !runtimeio.IsDeferredRecovery(err) && !(warm.Err() != nil && errors.Is(err, warm.Err())) {
				return err
			}
			log.Printf("Apple overdue sandbox %s deferred to background recovery: %v", row.ID, err)
		}
	}
	if warm.Err() != nil && ctx.Err() == nil {
		log.Printf("Apple startup warm-up budget exhausted; all durable work remains registered")
	}
	return ctx.Err()
}

func (c *Client) lockRecovery(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.mu.TryLock() {
			if c.closing {
				c.mu.Unlock()
				return errors.New("backend is shutting down")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func validateIntent(op database.Operation) error {
	if op.RuntimeKind != "container" || !validID(op.NativeID) {
		return errors.New("invalid recovery runtime or native identity")
	}
	switch op.Kind {
	case "create":
		if op.Token != op.NativeID {
			return errors.New("creation intent lacks exact ownership token")
		}
	case "start", "restart":
		if op.Deadline == nil {
			return errors.New("start/restart intent lacks an absolute deadline")
		}
	case "stop", "delete":
	default:
		return fmt.Errorf("unknown pending operation %q", op.Kind)
	}
	return nil
}

func (c *Client) retryOperation(op database.Operation) {
	if c.closing {
		return
	}
	check, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	current, err := c.repo.WithContext(check).FindOperation(op.ID)
	cancel()
	if err == nil && current == nil {
		return
	}
	if op.Kind != "create" && op.Deadline != nil && c.timers[op.NativeID] == nil {
		c.scheduleDeadline(op.NativeID, *op.Deadline, max(op.Deadline.Sub(c.now()), runtimeio.RecoveryRetryDelay))
	}
	c.queueOperation(op)
}

func (c *Client) CreationFailed(id string) {
	if op, ok := c.createAttempts.Load(id); ok {
		c.queueOperation(op.(database.Operation))
	}
}

func (c *Client) queueOperation(op database.Operation) {
	c.recovery.Schedule(op.ID, func(ctx context.Context) error {
		if err := c.lockRecovery(ctx); err != nil {
			return err
		}
		defer c.mu.Unlock()
		current, err := c.repo.WithContext(ctx).FindOperation(op.ID)
		if err != nil || current == nil {
			return err
		}
		if current.RuntimeKind != op.RuntimeKind || current.Token != op.Token {
			return errors.New("recovery attempt identity changed")
		}
		err = c.reconcile(ctx, *current)
		if err == nil && (current.Kind == "create" || current.Kind == "delete") {
			c.createAttempts.Delete(current.NativeID)
		}
		return err
	})
}

func (c *Client) reconcileResource(ctx context.Context, id string) error {
	op, err := c.repo.Pending("container", id)
	if err != nil || op == nil {
		return err
	}
	if op.Kind == "create" {
		return runtimeio.DeferRecovery(errors.New("creation awaits adoption or explicit failed-create recovery"))
	}
	return c.reconcile(ctx, *op)
}

func (c *Client) reconcile(ctx context.Context, op database.Operation) error {
	repo := c.repo.WithContext(ctx)
	if err := validateIntent(op); err != nil {
		return err
	}
	if err := repo.ValidateOperation(op); err != nil {
		return err
	}
	if op.Kind == "create" && op.Token != op.NativeID {
		return errors.New("creation intent lacks exact ownership token")
	}
	if !validID(op.NativeID) {
		return errors.New("invalid native recovery identity")
	}
	inventory, err := c.inventory(ctx)
	if err != nil {
		return err
	}
	info, found := inventory[op.NativeID]
	if found && info.Configuration.Labels[ownerLabel] != op.NativeID {
		return fmt.Errorf("native ownership label mismatch for %s; refusing recovery mutation", op.NativeID)
	}
	switch op.Kind {
	case "create", "delete":
		if found {
			if _, err := c.run(ctx, nil, "delete", "--force", op.NativeID); err != nil {
				return err
			}
		}
		c.clearCommands(op.NativeID)
		c.changed(op.NativeID)
		if err := repo.DeleteOperation(op); err != nil {
			return err
		}
		c.clearTimer(op.NativeID)
		delete(c.finished, op.NativeID)
		return nil
	case "stop":
		if found && info.Status.State != "stopped" {
			if _, err := c.run(ctx, nil, "stop", op.NativeID); err != nil {
				return err
			}
		}
		if err := repo.CompleteOperation(op, nil, nil); err != nil {
			return err
		}
		c.clearTimer(op.NativeID)
		c.changed(op.NativeID)
		return nil
	case "start", "restart":
		if op.Deadline == nil {
			return errors.New("start/restart intent lacks an absolute deadline")
		}
		if found && info.Status.State != "running" && info.Status.State != "stopped" {
			return runtimeio.DeferRecovery(fmt.Errorf("native state for %s is not settled; intent retained", op.NativeID))
		}
		deadline := op.Deadline
		ports := database.JSONMap{}
		if !found {
			deadline = nil
		} else {
			if info.Status.State != "running" {
				deadline = nil
			}
			mapped, err := networkPorts(info)
			if err != nil {
				return err
			}
			ports = database.JSONMap(mapped)
		}
		if err := repo.CompleteOperation(op, ports, deadline); err != nil {
			return err
		}
		c.changed(op.NativeID)
		if deadline != nil {
			c.scheduleDeadline(op.NativeID, *deadline, deadline.Sub(c.now()))
		} else {
			c.clearTimer(op.NativeID)
		}
		return nil
	default:
		return fmt.Errorf("unknown pending operation %q", op.Kind)
	}
}

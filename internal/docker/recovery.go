package docker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"

	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

const ownerLabel = "io.opensbx.attempt"

func (c *Client) AdoptCreation(ctx context.Context, row database.Sandbox) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	err := c.repo.UpdateProvenance(row, true)
	if err == nil {
		c.createAttempts.Delete(row.RuntimeID())
	}
	return err
}

func (c *Client) checkOwnership(ctx context.Context, op database.Operation) error {
	if op.Token == "" {
		return nil
	} // Legacy ownership is bound by its stored native ID.
	info, err := c.cli.ContainerInspect(ctx, op.NativeID, moby.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return runtimeio.DeferRecovery(err)
	}
	if info.Container.ID != op.NativeID || info.Container.Config == nil || info.Container.Config.Labels[ownerLabel] != op.Token {
		return fmt.Errorf("native ownership mismatch for %s", op.NativeID)
	}
	return nil
}

// Recover is explicitly invoked after server initialization, before listeners.
// It only visits durable intents and deadlines, never adopts runtime inventory.
func (c *Client) Recover(ctx context.Context) error {
	if err := c.lockLifecycle(ctx); err != nil {
		return err
	}
	defer c.lifecycleMu.Unlock()
	repo := c.repo.WithContext(ctx)
	ops, err := repo.Operations("docker")
	if err != nil {
		return err
	}
	rows, err := repo.FindAll()
	if err != nil {
		return err
	}
	// Register everything before spending any of the warm-up budget on native IO.
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
		if row.RuntimeKind != "docker" || row.ExpiresAt == nil {
			continue
		}
		delay := time.Until(*row.ExpiresAt)
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
			log.Printf("Docker startup intent %s deferred to background recovery: %v", op.ID, err)
		}
	}
	for _, row := range rows {
		if warm.Err() != nil {
			break
		}
		if row.RuntimeKind != "docker" || row.ExpiresAt == nil || row.ExpiresAt.After(time.Now()) {
			continue
		}
		pending, err := c.repo.Pending("docker", row.RuntimeID())
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
			log.Printf("Docker overdue sandbox %s deferred to background recovery: %v", row.ID, err)
		}
	}
	if warm.Err() != nil && ctx.Err() == nil {
		log.Printf("Docker startup warm-up budget exhausted; all durable work remains registered")
	}
	return ctx.Err()
}

func validateIntent(op database.Operation) error {
	if op.RuntimeKind != "docker" {
		return errors.New("recovery runtime mismatch")
	}
	switch op.Kind {
	case "create":
		if op.Token == "" || op.NativeName == "" {
			return errors.New("creation intent lacks exact name and ownership token")
		}
	case "start", "restart":
		if op.Deadline == nil || op.NativeID == "" {
			return errors.New("start/restart intent lacks identity or deadline")
		}
	case "stop", "delete":
		if op.NativeID == "" {
			return errors.New("recovery intent lacks native identity")
		}
	default:
		return fmt.Errorf("unknown pending operation %q", op.Kind)
	}
	return nil
}

// Called under lifecycleMu after failed work, or while registering startup work.
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
	if op.Kind != "create" && op.Deadline != nil && c.getTimerEntry(op.NativeID) == nil {
		c.scheduleDeadline(op.NativeID, *op.Deadline, max(time.Until(*op.Deadline), runtimeio.RecoveryRetryDelay))
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
		if err := c.lockLifecycle(ctx); err != nil {
			return err
		}
		defer c.lifecycleMu.Unlock()
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
	op, err := c.repo.Pending("docker", id)
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
	if op.Kind == "create" && (op.Token == "" || op.NativeName == "") {
		return errors.New("creation intent lacks exact name and ownership token")
	}
	ref := op.NativeID
	if op.Kind == "create" && ref == "" {
		ref = op.NativeName
		row, err := repo.FindByAttempt(op.RuntimeKind, op.Token)
		if err != nil {
			return err
		}
		if row != nil {
			if op.PublicID != "" && row.ID != op.PublicID {
				return errors.New("creation owner does not match attempt")
			}
			ref = row.RuntimeID()
			op.NativeID = ref
			op.PublicID = row.ID
		}
	}
	info, err := c.cli.ContainerInspect(ctx, ref, moby.ContainerInspectOptions{})
	if err != nil && !errdefs.IsNotFound(err) {
		return runtimeio.DeferRecovery(err)
	}
	missing := errdefs.IsNotFound(err)
	if !missing {
		if op.Token != "" && (info.Container.Config == nil || info.Container.Config.Labels[ownerLabel] != op.Token) {
			return fmt.Errorf("native ownership label mismatch for %s; refusing recovery mutation", ref)
		}
		if op.NativeID != "" && info.Container.ID != op.NativeID {
			return fmt.Errorf("native identity mismatch for %s", ref)
		}
		if info.Container.State == nil {
			return errors.New("native recovery state is missing")
		}
		if info.Container.State.Restarting {
			return runtimeio.DeferRecovery(fmt.Errorf("native state for %s is not settled", ref))
		}
		if op.Kind == "create" && op.NativeID == "" {
			op.NativeID = info.Container.ID
			if op.PublicID == "" {
				row, err := repo.FindByNativeID(op.NativeID)
				if err != nil {
					return err
				}
				if row != nil {
					if row.AttemptToken != op.Token || row.RuntimeKind != op.RuntimeKind {
						return errors.New("creation owner does not match attempt")
					}
					op.PublicID = row.ID
				}
			}
		}
	}
	switch op.Kind {
	case "create", "delete":
		if !missing {
			if _, err := c.cli.ContainerRemove(ctx, info.Container.ID, moby.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
				return runtimeio.DeferRecovery(err)
			}
		}
		c.clearCommands(op.NativeID)
		c.invalidateCache(op.NativeID)
		if err := repo.DeleteOperation(op); err != nil {
			return err
		}
		c.cancelTimer(op.NativeID)
		return nil
	case "stop":
		if !missing && info.Container.State.Running {
			if _, err := c.cli.ContainerStop(ctx, op.NativeID, moby.ContainerStopOptions{}); err != nil && !errdefs.IsNotFound(err) {
				return runtimeio.DeferRecovery(err)
			}
		}
		if err := repo.CompleteOperation(op, nil, nil); err != nil {
			return err
		}
		c.cancelTimer(op.NativeID)
		c.invalidateCache(op.NativeID)
		return nil
	case "start", "restart":
		if op.Deadline == nil {
			return errors.New("start/restart intent lacks an absolute deadline")
		}
		// A lost response cannot prove whether restart happened. Observe, do not
		// repeat it: stopped/missing resources remain stopped/missing.
		deadline := op.Deadline
		ports := database.JSONMap{}
		if missing {
			deadline = nil
		} else {
			if !info.Container.State.Running {
				deadline = nil
			}
			ports = database.JSONMap(extractPorts(info.Container.NetworkSettings.Ports))
		}
		if err := repo.CompleteOperation(op, ports, deadline); err != nil {
			return err
		}
		c.invalidateCache(op.NativeID)
		if deadline != nil {
			c.scheduleDeadline(op.NativeID, *deadline, time.Until(*deadline))
		} else {
			c.cancelTimer(op.NativeID)
		}
		return nil
	default:
		return fmt.Errorf("unknown pending operation %q", op.Kind)
	}
}

// Used by shutdown too: native success alone must not erase durable work.
func (c *Client) stopForShutdown(ctx context.Context, id string) error {
	if err := c.reconcileResource(ctx, id); err != nil {
		return err
	}
	row, err := c.repo.FindByNativeID(id)
	if err != nil || row == nil {
		return err
	}
	err = c.stopLocked(ctx, id)
	if errors.Is(err, sandbox.ErrAlreadyStopped) || errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	return err
}

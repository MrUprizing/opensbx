package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
)

func TestRecoverRegistersRetryForEveryOverdueSandboxBeforeCallerBudgetExpires(t *testing.T) {
	const firstID = "opensbx-11111111111111111111111111111111"
	const secondID = "opensbx-22222222222222222222222222222222"
	deadline := time.Now().Add(-time.Minute).UTC()
	db := database.New(filepath.Join(t.TempDir(), "overdue-budget.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbSQL.Close() })
	repo := database.NewRepository(db)
	for _, id := range []string{firstID, secondID} {
		if err := repo.CreateOwnership(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "container", AttemptToken: id, Name: id, ExpiresAt: &deadline}); err != nil {
			t.Fatal(err)
		}
	}
	stopEntered := make(chan struct{}, 2)
	runner := &synchronizedRunner{t: t}
	runner.run = func(ctx context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			first := strings.TrimSuffix(strings.TrimPrefix(listJSON(firstID, "running", "[]"), "["), "]")
			second := strings.TrimSuffix(strings.TrimPrefix(listJSON(secondID, "running", "[]"), "["), "]")
			_, _ = io.WriteString(out, "["+first+","+second+"]")
		case len(args) == 2 && args[0] == "stop":
			select {
			case stopEntered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		default:
			return fmt.Errorf("unexpected bounded recovery command %#v", args)
		}
		return nil
	}
	client := New(repo, runner, time.Now)
	t.Cleanup(func() {
		client.mu.Lock()
		for id := range client.timers {
			client.clearTimer(id)
		}
		client.mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	err = client.Recover(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recover() cancellation error=%v; caller budget exhaustion must remain actionable", err)
	}
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not begin a bounded overdue stop")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, id := range []string{firstID, secondID} {
		entry := client.timers[id]
		if entry == nil || !entry.at.Equal(deadline) {
			t.Errorf("overdue %s has no registered retry with original deadline %s; entry=%v", id, deadline, expirationDeadline(entry))
		}
	}
}

func TestRecoverInternalWarmupRegistersAllDeadlinesAndReturnsForListenerStartup(t *testing.T) {
	const firstID = "opensbx-33333333333333333333333333333333"
	const secondID = "opensbx-44444444444444444444444444444444"
	deadline := time.Now().Add(-time.Minute).UTC()
	db := database.New(filepath.Join(t.TempDir(), "warmup-budget.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbSQL.Close() })
	repo := database.NewRepository(db)
	for _, id := range []string{firstID, secondID} {
		if err := repo.CreateOwnership(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "container", AttemptToken: id, Name: id, ExpiresAt: &deadline}); err != nil {
			t.Fatal(err)
		}
	}
	stopEntered := make(chan struct{}, 2)
	runner := &synchronizedRunner{t: t}
	runner.run = func(ctx context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
		switch {
		case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
			first := strings.TrimSuffix(strings.TrimPrefix(listJSON(firstID, "running", "[]"), "["), "]")
			second := strings.TrimSuffix(strings.TrimPrefix(listJSON(secondID, "running", "[]"), "["), "]")
			_, _ = io.WriteString(out, "["+first+","+second+"]")
		case len(args) == 2 && args[0] == "stop":
			select {
			case stopEntered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		default:
			return fmt.Errorf("unexpected warmup command %#v", args)
		}
		return nil
	}
	client := New(repo, runner, time.Now)
	t.Cleanup(func() {
		client.mu.Lock()
		for id := range client.timers {
			client.clearTimer(id)
		}
		client.mu.Unlock()
	})

	callerCtx, cancel := context.WithTimeout(context.Background(), runtimeio.RecoveryWarmupBudget+2*time.Second)
	defer cancel()
	started := time.Now()
	if err := client.Recover(callerCtx); err != nil {
		t.Fatalf("transient native stop exceeded internal warmup and made startup fatal: %v", err)
	}
	if elapsed := time.Since(started); elapsed > runtimeio.RecoveryWarmupBudget+time.Second {
		t.Fatalf("Recover() exceeded its internal warmup budget: elapsed=%s budget=%s", elapsed, runtimeio.RecoveryWarmupBudget)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, id := range []string{firstID, secondID} {
		entry := client.timers[id]
		if entry == nil || !entry.at.Equal(deadline) {
			t.Errorf("startup warmup did not preserve retry for %s at original deadline %s; entry=%v", id, deadline, expirationDeadline(entry))
		}
	}
}

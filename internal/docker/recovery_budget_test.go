package docker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
)

func TestRecoverRegistersRetryForEveryOverdueSandboxBeforeCallerCancellation(t *testing.T) {
	db := database.New(filepath.Join(t.TempDir(), "overdue-budget.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbSQL.Close() })
	dc, fixture := newDockerFixture(t)
	dc.repo = database.NewRepository(db).NativeView()
	deadline := time.Now().Add(-time.Minute).UTC()
	for _, row := range []database.Sandbox{
		{ID: "public-a", NativeID: "container-1", RuntimeKind: "docker", Name: "first", ExpiresAt: &deadline},
		{ID: "public-b", NativeID: "container-2", RuntimeKind: "docker", Name: "second", ExpiresAt: &deadline},
	} {
		if err := dc.repo.CreateOwnership(row); err != nil {
			t.Fatal(err)
		}
	}
	fixture.mu.Lock()
	fixture.blockStopRequests = true
	fixture.stopEntered = make(chan struct{}, 2)
	fixture.mu.Unlock()
	t.Cleanup(func() {
		fixture.mu.Lock()
		fixture.blockStopRequests = false
		fixture.mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- dc.Recover(ctx) }()
	select {
	case <-fixture.stopEntered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("recovery did not reach the blocked overdue-stop request")
	}
	for _, nativeID := range []string{"container-1", "container-2"} {
		entry := dc.getTimerEntry(nativeID)
		if entry == nil || !entry.expiresAt.Equal(deadline) {
			t.Errorf("overdue %s has no registered retry with original deadline %s; entry=%v", nativeID, deadline, entryDeadline(entry))
		}
	}
	cancel()
	select {
	case err := <-recoveryDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Recover() after canceling the blocked caller returned %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recover() did not return after caller cancellation")
	}
}

func TestRecoverInternalWarmupRegistersAllDeadlinesAndReturnsForListenerStartup(t *testing.T) {
	db := database.New(filepath.Join(t.TempDir(), "warmup-budget.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbSQL.Close() })
	dc, fixture := newDockerFixture(t)
	dc.repo = database.NewRepository(db).NativeView()
	deadline := time.Now().Add(-time.Minute).UTC()
	for _, row := range []database.Sandbox{
		{ID: "public-a", NativeID: "container-1", RuntimeKind: "docker", Name: "first", ExpiresAt: &deadline},
		{ID: "public-b", NativeID: "container-2", RuntimeKind: "docker", Name: "second", ExpiresAt: &deadline},
	} {
		if err := dc.repo.CreateOwnership(row); err != nil {
			t.Fatal(err)
		}
	}
	fixture.mu.Lock()
	fixture.blockStopRequests = true
	fixture.stopEntered = make(chan struct{}, 2)
	fixture.mu.Unlock()
	t.Cleanup(func() {
		fixture.mu.Lock()
		fixture.blockStopRequests = false
		fixture.mu.Unlock()
	})

	callerCtx, cancel := context.WithTimeout(context.Background(), runtimeio.RecoveryWarmupBudget+2*time.Second)
	defer cancel()
	started := time.Now()
	if err := dc.Recover(callerCtx); err != nil {
		t.Fatalf("transient native stop exceeded internal warmup and made startup fatal: %v", err)
	}
	if elapsed := time.Since(started); elapsed > runtimeio.RecoveryWarmupBudget+time.Second {
		t.Fatalf("Recover() exceeded its internal warmup budget: elapsed=%s budget=%s", elapsed, runtimeio.RecoveryWarmupBudget)
	}
	for _, nativeID := range []string{"container-1", "container-2"} {
		entry := dc.getTimerEntry(nativeID)
		if entry == nil || !entry.expiresAt.Equal(deadline) {
			t.Errorf("startup warmup did not preserve retry for %s at original deadline %s; entry=%v", nativeID, deadline, entryDeadline(entry))
		}
	}
}

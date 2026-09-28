package docker

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"opensbx/internal/database"
	"opensbx/internal/runtimeio"

	"gorm.io/gorm"
)

func seedDockerLifecycleOwner(t *testing.T, c *Client, id string) {
	t.Helper()
	if err := c.repo.Save(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "docker", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleFailuresPreserveExpiration(t *testing.T) {
	for _, operation := range []string{"stop", "restart", "remove"} {
		t.Run(operation, func(t *testing.T) {
			c, fixture := newDockerFixture(t)
			seedDockerLifecycleOwner(t, c, "container-1")
			c.scheduleStop("container-1", 60)
			original := c.getTimerEntry("container-1")
			fixture.mu.Lock()
			fixture.fail["POST /containers/container-1/"+operation] = http.StatusInternalServerError
			fixture.fail["DELETE /containers/container-1"] = http.StatusInternalServerError
			fixture.mu.Unlock()
			var err error
			switch operation {
			case "stop":
				err = c.Stop(context.Background(), "container-1")
			case "restart":
				_, err = c.Restart(context.Background(), "container-1")
			case "remove":
				err = c.Remove(context.Background(), "container-1")
			}
			if err == nil || c.getTimerEntry("container-1") != original {
				t.Fatalf("failed %s must retain the original expiration; err=%v", operation, err)
			}
		})
	}
}

func TestConcurrentRenewalsRetireEverySupersededTimer(t *testing.T) {
	c, fixture := newDockerFixture(t)
	seedDockerLifecycleOwner(t, c, "container-1")
	c.scheduleStop("container-1", 60)
	original := c.getTimerEntry("container-1")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.RenewExpiration(context.Background(), "container-1", 120); err != nil {
				t.Errorf("renew: %v", err)
			}
		}()
	}
	wg.Wait()
	current := c.getTimerEntry("container-1")
	select {
	case <-original.cancel:
	default:
		t.Fatal("superseded timer was not canceled")
	}
	// Simulate a callback that had already received its timer tick before renewal.
	c.expire("container-1", original)
	if c.getTimerEntry("container-1") != current {
		t.Fatal("stale callback replaced current expiration")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, request := range fixture.requests {
		if request == "POST /containers/container-1/stop" {
			t.Fatal("superseded expiration stopped the sandbox")
		}
	}
}

func TestTimerReplacementCancelsPreviousGeneration(t *testing.T) {
	c, _ := newDockerFixture(t)
	c.scheduleStop("container-1", 60)
	for range 16 {
		old := c.getTimerEntry("container-1")
		c.scheduleStop("container-1", 120)
		select {
		case <-old.cancel:
		default:
			t.Fatal("replacing a timer left its old generation active")
		}
	}
}

func TestExpirationRetriesFailureAndClearsAfterSuccess(t *testing.T) {
	c, fixture := newDockerFixture(t)
	seedDockerLifecycleOwner(t, c, "container-1")
	c.scheduleStop("container-1", 60)
	original := c.getTimerEntry("container-1")
	fixture.mu.Lock()
	fixture.fail["POST /containers/container-1/stop"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	c.expire("container-1", original)
	retry := c.getTimerEntry("container-1")
	if retry == nil || retry == original || !retry.expiresAt.Equal(original.expiresAt) {
		t.Fatal("failed expiration must retain the persisted deadline while tracking a separate retry timer")
	}
	fixture.mu.Lock()
	delete(fixture.fail, "POST /containers/container-1/stop")
	fixture.mu.Unlock()
	c.expire("container-1", retry)
	if c.getTimerEntry("container-1") != nil {
		t.Fatal("successful expiration retained tracking")
	}
}

func TestShutdownConcurrentCancellationAndRenewal(t *testing.T) {
	c, _ := newDockerFixture(t)
	seedDockerLifecycleOwner(t, c, "container-1")
	c.scheduleStop("container-1", 60)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.cancelTimer("container-1")
			_ = c.RenewExpiration(context.Background(), "container-1", 60)
		}()
	}
	c.Shutdown(context.Background())
	wg.Wait()
	if c.getTimerEntry("container-1") != nil {
		t.Fatal("renewal installed a timer after shutdown")
	}
}

func TestLifecycleLockWaitRespectsCancellation(t *testing.T) {
	c, _ := newDockerFixture(t)
	c.lifecycleMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.Stop(ctx, "container-1")
	c.Shutdown(ctx)
	c.lifecycleMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait: %v", err)
	}
}

func TestRemovePersistenceFailuresRemainRetryable(t *testing.T) {
	for _, table := range []string{"commands", "sandboxes"} {
		t.Run(table, func(t *testing.T) {
			c, fixture := newDockerFixture(t)
			db := database.New(":memory:")
			c.repo = database.NewRepository(db).NativeView()
			if err := c.repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", Name: "demo"}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected deletion failure")
			if err := db.Callback().Delete().Before("gorm:delete").Register("test:delete-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.Remove(context.Background(), "container-1"); !errors.Is(err, failure) {
				t.Fatalf("deletion must report persistence failure: %v", err)
			}
			row, err := c.repo.FindByID("container-1")
			if err != nil || row == nil {
				t.Fatalf("ownership must remain for retry: row=%v err=%v", row, err)
			}
			if err := db.Callback().Delete().Remove("test:delete-failure"); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			fixture.fail["DELETE /containers/container-1"] = http.StatusNotFound
			fixture.mu.Unlock()
			if err := c.Remove(context.Background(), "container-1"); err != nil {
				t.Fatalf("retry after native deletion: %v", err)
			}
			row, err = c.repo.FindByID("container-1")
			if err != nil || row != nil {
				t.Fatalf("retry left ownership: row=%v err=%v", row, err)
			}
		})
	}
}

func TestRemoveDoesNotLeaveCommandHistoryWhenSandboxDeletionFails(t *testing.T) {
	c, _ := newDockerFixture(t)
	db := database.New(":memory:")
	c.repo = database.NewRepository(db).NativeView()
	if err := c.repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := c.repo.SaveCommand(database.Command{ID: "cmd-1", SandboxID: "container-1", Name: "echo"}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected sandbox deletion failure")
	if err := db.Callback().Delete().Before("gorm:delete").Register("test:sandbox-delete-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "sandboxes" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(context.Background(), "container-1"); !errors.Is(err, failure) {
		t.Fatalf("Remove() error = %v; want sandbox deletion failure", err)
	}
	commands, err := database.NewRepository(db).FindCommandsBySandbox("public-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].ID != "cmd-1" {
		t.Fatalf("command history after failed metadata deletion = %+v; want original command retained", commands)
	}
	ops, err := c.repo.Operations("docker")
	if err != nil || len(ops) != 1 || ops[0].Kind != "delete" {
		t.Fatalf("delete intent after failed metadata transaction=%+v err=%v", ops, err)
	}
	row, err := c.repo.FindByID("container-1")
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("sandbox ownership was lost after the transaction failed")
	}
	if err := db.Callback().Delete().Remove("test:sandbox-delete-failure"); err != nil {
		t.Fatal(err)
	}
	if err := c.recovery.Run(context.Background(), ops[0].ID); err != nil {
		t.Fatalf("same-process retry worker = %v", err)
	}
	ops, err = c.repo.Operations("docker")
	if err != nil || len(ops) != 0 {
		t.Fatalf("successful retry retained delete intent=%+v err=%v", ops, err)
	}
	row, err = c.repo.FindByID("container-1")
	if err != nil || row != nil {
		t.Fatalf("same-process delete worker left ownership row=%+v err=%v", row, err)
	}
	commands, err = database.NewRepository(db).FindCommandsBySandbox("public-id")
	if err != nil || len(commands) != 0 {
		t.Fatalf("same-process delete worker left history=%+v err=%v", commands, err)
	}
}

func TestExecCommandAndRemoveDoNotLeaveOrphanHistory(t *testing.T) {
	// Concurrent requests must share one SQLite schema; :memory: may open a
	// separate database per pooled connection under the race detector.
	db := database.New(filepath.Join(t.TempDir(), "exec-remove.db"))
	dbSQL, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	dbSQL.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = dbSQL.Close() })
	c, fixture := newDockerFixture(t)
	c.repo = database.NewRepository(db).NativeView()
	if err := c.repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	fixture.execCreateEntered = make(chan struct{})
	fixture.releaseExecCreate = make(chan struct{})
	execDone := make(chan error, 1)
	go func() {
		_, err := c.ExecCommand(context.Background(), "container-1", runtimeio.ExecCommandRequest{Command: "echo"})
		execDone <- err
	}()
	select {
	case <-fixture.execCreateEntered:
	case <-time.After(2 * time.Second):
		close(fixture.releaseExecCreate)
		t.Fatal("ExecCommand did not reach exec-create barrier")
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- c.Remove(context.Background(), "container-1") }()
	// On the unfixed implementation Remove completes while ExecCreate is held.
	// With lifecycle serialization it waits, so release ExecCreate after a bounded
	// observation window and verify that removal runs after command persistence.
	removeCompletedBeforeExecCreate := false
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("Remove() = %v", err)
		}
		removeCompletedBeforeExecCreate = true
	case <-time.After(2 * time.Second):
	}
	close(fixture.releaseExecCreate)
	select {
	case err := <-execDone:
		if err != nil {
			t.Fatalf("ExecCommand() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecCommand did not finish after releasing command persistence")
	}
	if !removeCompletedBeforeExecCreate {
		select {
		case err := <-removeDone:
			if err != nil {
				t.Fatalf("Remove() after command save = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Remove did not finish after command save was released")
		}
	}
	commands, err := database.NewRepository(db).FindCommandsBySandbox("container-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("Remove left orphan command history %+v (remove completed before ExecCreate response: %t)", commands, removeCompletedBeforeExecCreate)
	}
}

func TestStartAndRestartReportPortPersistenceFailures(t *testing.T) {
	for _, operation := range []string{"start", "restart"} {
		t.Run(operation, func(t *testing.T) {
			c, fixture := newDockerFixture(t)
			db := database.New(":memory:")
			c.repo = database.NewRepository(db).NativeView()
			if err := c.repo.CreateOwnership(database.Sandbox{ID: "public-id", NativeID: "container-1", Name: "demo"}); err != nil {
				t.Fatal(err)
			}
			if operation == "start" {
				fixture.mu.Lock()
				fixture.running = false
				fixture.mu.Unlock()
			}
			fixture.mu.Lock()
			fixture.containerHostPort = "40000"
			fixture.mu.Unlock()
			failure := errors.New("injected port persistence failure")
			if err := db.Callback().Update().Before("gorm:update").Register("test:port-update-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "sandboxes" {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "start" {
				// Start requires a stopped container.
				_, err = c.Start(context.Background(), "container-1")
			} else {
				_, err = c.Restart(context.Background(), "container-1")
			}
			if !errors.Is(err, failure) {
				t.Fatalf("%s() error = %v; want port persistence failure", operation, err)
			}
			if err := db.Callback().Update().Remove("test:port-update-failure"); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			requestCountBeforeRecover := len(fixture.requests)
			fixture.mu.Unlock()
			if err := c.Recover(context.Background()); err != nil {
				t.Fatalf("Recover() should observe native state and finish the pending operation: %v", err)
			}
			row, err := database.NewRepository(db).FindByID("public-id")
			if err != nil || row == nil || row.Ports["3000/tcp"] != "40000" {
				var ports database.JSONMap
				if row != nil {
					ports = row.Ports
				}
				t.Fatalf("recovered port mapping=%v row=%+v err=%v", ports, row, err)
			}
			ops, err := c.repo.Operations("docker")
			if err != nil || len(ops) != 0 {
				t.Fatalf("pending lifecycle intent after recovery=%+v err=%v", ops, err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			mutations := 0
			for _, request := range fixture.requests[requestCountBeforeRecover:] {
				if request == "POST /containers/container-1/start" || request == "POST /containers/container-1/restart" {
					mutations++
				}
			}
			if mutations != 0 {
				t.Fatalf("recovery replayed a start/restart mutation: %v", fixture.requests[requestCountBeforeRecover:])
			}
		})
	}
}

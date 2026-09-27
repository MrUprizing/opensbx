package docker

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"opensbx/internal/database"

	"gorm.io/gorm"
)

func TestLifecycleFailuresPreserveExpiration(t *testing.T) {
	for _, operation := range []string{"stop", "restart", "remove"} {
		t.Run(operation, func(t *testing.T) {
			c, fixture := newDockerFixture(t)
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
	c.scheduleStop("container-1", 60)
	original := c.getTimerEntry("container-1")
	fixture.mu.Lock()
	fixture.fail["POST /containers/container-1/stop"] = http.StatusInternalServerError
	fixture.mu.Unlock()
	c.expire("container-1", original)
	retry := c.getTimerEntry("container-1")
	if retry == nil || retry == original || time.Until(retry.expiresAt) > 30*time.Second {
		t.Fatal("failed expiration must install a bounded retry")
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

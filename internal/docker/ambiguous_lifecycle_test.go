package docker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"opensbx/internal/database"
)

func TestAmbiguousStartAndRestartScheduleSameProcessRecovery(t *testing.T) {
	for _, operation := range []string{"start", "restart"} {
		t.Run(operation, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			db := database.New(":memory:")
			dc.repo = database.NewRepository(db).NativeView()
			seedDockerLifecycleOwner(t, dc, "container-1")
			fixture.mu.Lock()
			fixture.running = operation == "restart"
			fixture.failAfterMutation = map[string]int{
				"POST /containers/container-1/" + operation: http.StatusInternalServerError,
			}
			fixture.mu.Unlock()

			var err error
			if operation == "start" {
				_, err = dc.Start(context.Background(), "container-1")
			} else {
				_, err = dc.Restart(context.Background(), "container-1")
			}
			if err == nil {
				t.Fatal("native mutation response loss was not reported")
			}
			fixture.mu.Lock()
			running := fixture.running
			fixture.mu.Unlock()
			if !running {
				t.Fatal("fixture did not apply the native mutation before losing its response")
			}

			var expected time.Time
			ops, opErr := dc.repo.Operations("docker")
			if opErr != nil {
				t.Fatal(opErr)
			} else if len(ops) == 1 && ops[0].Deadline != nil {
				expected = *ops[0].Deadline
			} else if row, rowErr := database.NewRepository(db).FindByID("container-1"); rowErr == nil && row != nil && row.ExpiresAt != nil {
				expected = *row.ExpiresAt
			}
			if expected.IsZero() {
				t.Fatal("ambiguous lifecycle mutation lost its durable absolute deadline")
			}
			waitForDockerDeadlineRecovery(t, dc, "container-1", expected)
			if len(ops) == 1 {
				if err := dc.recovery.Run(context.Background(), ops[0].ID); err != nil {
					t.Fatalf("same-process recovery worker = %v", err)
				}
			} else if len(ops) != 0 {
				t.Fatalf("unexpected pending operations: %+v", ops)
			}
			ops, err = dc.repo.Operations("docker")
			if err != nil || len(ops) != 0 {
				t.Fatalf("same-process worker did not reconcile the intent: ops=%+v err=%v", ops, err)
			}
			row, err := database.NewRepository(db).FindByID("container-1")
			if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(expected) {
				t.Fatalf("same-process recovery changed deadline: row=%+v err=%v", row, err)
			}

			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			mutations := 0
			for _, request := range fixture.requests {
				if request == "POST /containers/container-1/"+operation {
					mutations++
				}
			}
			if mutations != 1 {
				t.Fatalf("same-process recovery replayed %s: requests=%v", operation, fixture.requests)
			}
		})
	}
}

func TestRecoveryWorkerIgnoresMissingIntentAndRejectsNewerAttempt(t *testing.T) {
	for _, mode := range []string{"missing", "newer"} {
		t.Run(mode, func(t *testing.T) {
			dc, fixture := newDockerFixture(t)
			db := database.New(":memory:")
			dc.repo = database.NewRepository(db).NativeView()
			seedDockerLifecycleOwner(t, dc, "container-1")
			fixture.mu.Lock()
			fixture.running = false
			fixture.failAfterMutation = map[string]int{"POST /containers/container-1/start": http.StatusInternalServerError}
			fixture.mu.Unlock()
			if _, err := dc.Start(context.Background(), "container-1"); err == nil {
				t.Fatal("expected ambiguous native start failure")
			}
			ops, err := dc.repo.Operations("docker")
			if err != nil || len(ops) != 1 {
				t.Fatalf("pending start intent=%+v err=%v", ops, err)
			}
			op := ops[0]
			switch mode {
			case "missing":
				if err := dc.repo.ClearOperation(op); err != nil {
					t.Fatal(err)
				}
			case "newer":
				if err := db.Model(&database.Operation{}).Where("id = ?", op.ID).Update("token", "newer-attempt-token").Error; err != nil {
					t.Fatal(err)
				}
			}
			fixture.mu.Lock()
			before := append([]string(nil), fixture.requests...)
			fixture.mu.Unlock()
			err = dc.recovery.Run(context.Background(), op.ID)
			if mode == "missing" && err != nil {
				t.Fatalf("worker should treat a completed/missing intent as harmless: %v", err)
			}
			if mode == "newer" && (err == nil || !strings.Contains(err.Error(), "attempt identity changed")) {
				t.Fatalf("worker must reject a newer attempt identity: %v", err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if len(fixture.requests) != len(before) {
				t.Fatalf("stale recovery worker made native calls for %s intent: before=%v after=%v", mode, before, fixture.requests)
			}
		})
	}
}

func waitForDockerDeadlineRecovery(t *testing.T, c *Client, id string, expected time.Time) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		entry := c.getTimerEntry(id)
		if entry != nil && entry.expiresAt.Equal(expected) {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			var actual any
			if entry != nil {
				actual = entry.expiresAt
			}
			t.Fatalf("same-process %s recovery did not schedule the unchanged deadline %s; timer=%v", id, expected, actual)
		}
	}
}

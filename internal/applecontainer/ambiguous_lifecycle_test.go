package applecontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"opensbx/internal/database"
)

func TestRecoveryWorkerRechecksMissingAndNewerAppleIntents(t *testing.T) {
	for _, mode := range []string{"missing", "newer"} {
		t.Run(mode, func(t *testing.T) {
			const id = "opensbx-ffffffffffffffffffffffffffffffff"
			state := "stopped"
			runner := &synchronizedRunner{t: t}
			runner.run = func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
				switch {
				case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
					_, _ = io.WriteString(out, listJSON(id, state, "[]"))
				case reflect.DeepEqual(args, []string{"start", id}):
					state = "running"
					return errors.New("start response lost")
				default:
					return fmt.Errorf("unexpected stale-worker native call %#v", args)
				}
				return nil
			}
			path := filepath.Join(t.TempDir(), "stale-worker.db")
			db := database.New(path)
			repo := database.NewRepository(db)
			if err := repo.CreateOwnership(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "container", AttemptToken: id, Name: "stale-worker"}); err != nil {
				t.Fatal(err)
			}
			client := New(repo, runner, time.Now)
			t.Cleanup(func() {
				client.recovery.Stop()
				client.mu.Lock()
				for timerID := range client.timers {
					client.clearTimer(timerID)
				}
				client.mu.Unlock()
			})
			if _, err := client.Start(context.Background(), id); err == nil {
				t.Fatal("expected ambiguous start error")
			}
			ops, err := repo.Operations("container")
			if err != nil || len(ops) != 1 {
				t.Fatalf("queued start intent=%+v err=%v", ops, err)
			}
			op := ops[0]
			if mode == "missing" {
				if err := repo.ClearOperation(op); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Model(&database.Operation{}).Where("id = ?", op.ID).Update("token", "newer-token").Error; err != nil {
				t.Fatal(err)
			}
			callsBefore := len(runner.calls)
			err = client.recovery.Run(context.Background(), op.ID)
			if mode == "missing" && err != nil {
				t.Fatalf("completed/missing intent should be harmless: %v", err)
			}
			if mode == "newer" && (err == nil || !strings.Contains(err.Error(), "attempt identity changed")) {
				t.Fatalf("newer attempt should be rejected before native work: %v", err)
			}
			if len(runner.calls) != callsBefore {
				t.Fatalf("stale worker performed native I/O: before=%v after=%v", callsBefore, runner.calls)
			}
		})
	}
}

func TestAmbiguousStartAndRestartScheduleSameProcessRecovery(t *testing.T) {
	for _, operation := range []string{"start", "restart"} {
		t.Run(operation, func(t *testing.T) {
			const id = "opensbx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			base := time.Date(2038, 2, 3, 4, 5, 6, 0, time.UTC)
			state := "stopped"
			if operation == "restart" {
				state = "running"
			}
			var stateMu sync.Mutex
			startCalls, stopCalls := 0, 0
			runner := &synchronizedRunner{t: t}
			runner.run = func(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
				stateMu.Lock()
				defer stateMu.Unlock()
				switch {
				case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
					_, _ = io.WriteString(out, listJSON(id, state, "[]"))
				case reflect.DeepEqual(args, []string{"stop", id}):
					state = "stopped"
					stopCalls++
				case reflect.DeepEqual(args, []string{"start", id}):
					state = "running"
					startCalls++
					return errors.New("native start completed but its response was lost")
				default:
					return fmt.Errorf("unexpected lifecycle command %#v", args)
				}
				return nil
			}
			path := filepath.Join(t.TempDir(), "ambiguous.db")
			db := database.New(path)
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			repo := database.NewRepository(db)
			if err := repo.CreateOwnership(database.Sandbox{ID: id, NativeID: id, RuntimeKind: "container", AttemptToken: id, Name: "ambiguous"}); err != nil {
				t.Fatal(err)
			}
			client := New(repo, runner, func() time.Time { return base })
			t.Cleanup(func() {
				client.mu.Lock()
				for key := range client.timers {
					client.clearTimer(key)
				}
				client.mu.Unlock()
			})

			var mutationErr error
			if operation == "start" {
				_, mutationErr = client.Start(context.Background(), id)
			} else {
				_, mutationErr = client.Restart(context.Background(), id)
			}
			if mutationErr == nil {
				t.Fatal("ambiguous native mutation error was suppressed")
			}
			stateMu.Lock()
			observedState := state
			observedStarts, observedStops := startCalls, stopCalls
			stateMu.Unlock()
			if observedState != "running" || observedStarts != 1 || operation == "restart" && observedStops != 1 {
				t.Fatalf("fixture did not model exactly one applied %s mutation: state=%s starts=%d stops=%d", operation, observedState, observedStarts, observedStops)
			}

			ops, err := repo.Operations("container")
			if err != nil {
				t.Fatal(err)
			}
			var expected time.Time
			if len(ops) == 1 && ops[0].Deadline != nil {
				expected = *ops[0].Deadline
			} else if row, rowErr := repo.FindByID(id); rowErr == nil && row != nil && row.ExpiresAt != nil {
				expected = *row.ExpiresAt
			}
			if expected.IsZero() {
				t.Fatal("ambiguous lifecycle mutation lost its durable absolute deadline")
			}
			waitForAppleDeadlineRecovery(t, client, repo, id, expected)
			if len(ops) == 1 {
				if err := client.recovery.Run(context.Background(), ops[0].ID); err != nil {
					t.Fatalf("same-process recovery worker = %v", err)
				}
			} else if len(ops) != 0 {
				t.Fatalf("unexpected pending operations: %+v", ops)
			}
			ops, err = repo.Operations("container")
			if err != nil || len(ops) != 0 {
				t.Fatalf("same-process worker did not reconcile intent: ops=%+v err=%v", ops, err)
			}
			row, err := repo.FindByID(id)
			if err != nil || row == nil || row.ExpiresAt == nil || !row.ExpiresAt.Equal(expected) {
				t.Fatalf("same-process recovery changed deadline: row=%+v err=%v", row, err)
			}
		})
	}
}

func waitForAppleDeadlineRecovery(t *testing.T, c *Client, repo *database.Repository, id string, expected time.Time) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.Lock()
		entry := c.timers[id]
		var scheduled time.Time
		if entry != nil {
			scheduled = entry.at
		}
		c.mu.Unlock()
		if !scheduled.IsZero() && scheduled.Equal(expected) {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("same-process %s recovery did not schedule unchanged deadline %s; timer=%v", id, expected, scheduled)
		}
	}
}

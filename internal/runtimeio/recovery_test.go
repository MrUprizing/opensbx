package runtimeio

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRecoveryQueueAutomaticallyRetriesAfterThirtySeconds(t *testing.T) {
	var queue RecoveryQueue
	t.Cleanup(func() { queue.Stop() })
	type result struct {
		key string
		at  time.Time
	}
	completed := make(chan result, 2)
	queuedAt := time.Now()
	for _, key := range []string{"delayed-a", "delayed-b"} {
		key := key
		queue.Schedule(key, func(context.Context) error {
			completed <- result{key: key, at: time.Now()}
			return nil
		})
	}
	deadline := time.NewTimer(RecoveryRetryDelay + 5*time.Second)
	defer deadline.Stop()
	seen := map[string]time.Time{}
	for len(seen) != 2 {
		select {
		case item := <-completed:
			if item.at.Sub(queuedAt) < RecoveryRetryDelay-100*time.Millisecond {
				t.Fatalf("recovery %s ran before the configured 30s retry interval: %s", item.key, item.at.Sub(queuedAt))
			}
			seen[item.key] = item.at
		case <-deadline.C:
			t.Fatalf("queue timer did not automatically run every scheduled job: %v", seen)
		}
	}
	for _, key := range []string{"delayed-a", "delayed-b"} {
		if _, ok := seen[key]; !ok {
			t.Fatalf("scheduled intent %q did not run", key)
		}
	}
	queue.mu.Lock()
	remaining := len(queue.jobs)
	queue.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("successful scheduled work remained queued: jobs=%d", remaining)
	}
}

func TestRecoveryQueueShutdownCancelsRunningAndQueuedJobsWithoutRevival(t *testing.T) {
	var queue RecoveryQueue
	entered := make(chan struct{})
	finished := make(chan error, 1)
	var once sync.Once
	queue.Schedule("active", func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		finished <- ctx.Err()
		return ctx.Err()
	})
	queue.Schedule("waiting", func(context.Context) error {
		t.Error("shutdown revived a queued recovery job")
		return nil
	})
	runDone := make(chan error, 1)
	go func() { runDone <- queue.Run(context.Background(), "active") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("recovery work did not start")
	}
	queue.Stop()
	select {
	case err := <-finished:
		if err != context.Canceled {
			t.Fatalf("shutdown cancellation=%v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel active recovery work")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("active recovery worker did not exit after shutdown")
	}
	if err := queue.Run(context.Background(), "waiting"); err != nil {
		t.Fatalf("Run() after Stop() = %v", err)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.jobs) != 0 {
		t.Fatalf("shutdown retained/revived queue jobs: %v", queue.jobs)
	}
}

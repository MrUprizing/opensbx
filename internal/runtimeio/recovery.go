package runtimeio

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const (
	RecoveryRetryDelay    = 30 * time.Second
	RecoveryAttemptBudget = 2 * time.Second
	RecoveryWarmupBudget  = time.Second
	recoveryYield         = 100 * time.Millisecond
)

type deferredRecovery struct{ error }

func (e deferredRecovery) Unwrap() error { return e.error }

// DeferRecovery marks an operational runtime failure, not a DB/ownership error.
func DeferRecovery(err error) error {
	if err == nil {
		return nil
	}
	return deferredRecovery{err}
}
func IsDeferredRecovery(err error) bool {
	var deferred deferredRecovery
	return errors.As(err, &deferred)
}

type recoveryJob struct {
	work   func(context.Context) error
	timer  *time.Timer
	cancel context.CancelFunc
}

// RecoveryQueue is zero-value ready. Jobs are registered only for failed work
// or startup recovery, never for a live creation waiting for successful Adopt.
// No queue lock is held during work. Each attempt includes lock acquisition in
// its budget, preventing a burst of jobs from monopolizing a runtime lifecycle.
type RecoveryQueue struct {
	mu          sync.Mutex
	jobs        map[string]*recoveryJob
	closed      bool
	running     bool
	nextAttempt time.Time
}

func (q *RecoveryQueue) Schedule(key string, work func(context.Context) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.jobs == nil {
		q.jobs = make(map[string]*recoveryJob)
	}
	if q.jobs[key] != nil {
		return
	}
	job := &recoveryJob{work: work}
	q.jobs[key] = job
	job.timer = time.AfterFunc(RecoveryRetryDelay, func() { q.Run(context.Background(), key) })
}

// Run executes registered work without waiting for its retry timer; in-package
// tests can use it outside the client's lifecycle lock to exercise the worker.
// Background retries never inherit the failed request's cancellation.
func (q *RecoveryQueue) Run(parent context.Context, key string) error {
	for {
		q.mu.Lock()
		job := q.jobs[key]
		if q.closed || job == nil || job.cancel != nil {
			q.mu.Unlock()
			return nil
		}
		job.timer.Stop()
		// Dispatch one bounded attempt at a time with an API-access window between
		// attempts. Waiting jobs retain fresh budgets instead of all timing out
		// behind the same stalled resource on every retry cycle.
		if q.running || time.Now().Before(q.nextAttempt) {
			q.mu.Unlock()
			select {
			case <-time.After(recoveryYield):
				continue
			case <-parent.Done():
				q.mu.Lock()
				if !q.closed && q.jobs[key] == job && job.cancel == nil {
					job.timer = time.AfterFunc(RecoveryRetryDelay, func() { q.Run(context.Background(), key) })
				}
				q.mu.Unlock()
				return parent.Err()
			}
		}
		q.running = true
		ctx, cancel := context.WithTimeout(parent, RecoveryAttemptBudget)
		job.cancel = cancel
		q.mu.Unlock()
		err := job.work(ctx)
		cancel()
		q.mu.Lock()
		defer q.mu.Unlock()
		q.running = false
		q.nextAttempt = time.Now().Add(recoveryYield)
		job.cancel = nil
		if q.closed || q.jobs[key] != job {
			return err
		}
		if err == nil {
			delete(q.jobs, key)
			return nil
		}
		log.Printf("recovery %s deferred: %v; durable work retained, retry in %s", key, err, RecoveryRetryDelay)
		job.timer = time.AfterFunc(RecoveryRetryDelay, func() { q.Run(context.Background(), key) })
		return err
	}
}

// Stop cancels active attempts before callers wait for the lifecycle lock.
func (q *RecoveryQueue) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	for key, job := range q.jobs {
		job.timer.Stop()
		if job.cancel != nil {
			job.cancel()
		}
		delete(q.jobs, key)
	}
}

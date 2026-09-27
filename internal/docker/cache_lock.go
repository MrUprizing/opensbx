package docker

import (
	"context"
	"sync"
)

type cacheLockEntry struct {
	held chan struct{}
	refs int // Owner plus registered waiters, protected by cacheLocks.mu.
}

type cacheLocks struct {
	mu      sync.Mutex
	entries map[string]*cacheLockEntry
}

// acquire serializes one private reference across clients without blocking other
// references. On success the caller must release exactly once. Cancellation never
// returns ownership, including when cancellation races with acquisition.
func (l *cacheLocks) acquire(ctx context.Context, ref string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*cacheLockEntry)
	}
	entry := l.entries[ref]
	if entry == nil {
		entry = &cacheLockEntry{held: make(chan struct{}, 1)}
		l.entries[ref] = entry
	}
	entry.refs++
	l.mu.Unlock()

	select {
	case entry.held <- struct{}{}:
	case <-ctx.Done():
		l.drop(ref, entry, false)
		return nil, ctx.Err()
	}
	release := func() { l.drop(ref, entry, true) }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (l *cacheLocks) drop(ref string, entry *cacheLockEntry, owned bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if owned {
		<-entry.held
	}
	entry.refs--
	// Registration and removal share the mutex: a key cannot be replaced while
	// an owner or waiter still holds a reference to its previous entry.
	if entry.refs == 0 {
		delete(l.entries, ref)
	}
}

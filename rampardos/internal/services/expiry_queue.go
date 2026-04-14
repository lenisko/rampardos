package services

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ExpiryQueue tracks files that should be deleted after a TTL.
// A single background goroutine sweeps the queue periodically,
// removing expired files and their cache index entries. Much more
// efficient than spawning a goroutine per request.
type ExpiryQueue struct {
	mu      sync.Mutex
	entries []expiryEntry
	ctx     context.Context
	cancel  context.CancelFunc
}

type expiryEntry struct {
	paths     []string  // files to delete (path + basePath)
	expiresAt time.Time
	onExpiry  func()    // optional callback (e.g. remove from cache index)
}

// GlobalExpiryQueue is the shared instance.
var GlobalExpiryQueue *ExpiryQueue

// InitExpiryQueue creates the global queue and starts the sweeper.
func InitExpiryQueue(sweepInterval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &ExpiryQueue{ctx: ctx, cancel: cancel}
	GlobalExpiryQueue = q
	go q.sweepLoop(sweepInterval)
}

// Add queues one or more file paths for deletion after ttl.
// The optional onExpiry callback runs after files are deleted
// (e.g. to remove cache index entries).
func (q *ExpiryQueue) Add(ttl time.Duration, onExpiry func(), paths ...string) {
	q.mu.Lock()
	q.entries = append(q.entries, expiryEntry{
		paths:     paths,
		expiresAt: time.Now().Add(ttl),
		onExpiry:  onExpiry,
	})
	q.mu.Unlock()
}

// Stop cancels the background sweeper.
func (q *ExpiryQueue) Stop() {
	q.cancel()
}

func (q *ExpiryQueue) sweepLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			q.sweep()
		case <-q.ctx.Done():
			return
		}
	}
}

func (q *ExpiryQueue) sweep() {
	now := time.Now()

	q.mu.Lock()
	// Filter in-place to avoid allocating a new slice every sweep.
	n := 0
	var expired []expiryEntry
	for _, e := range q.entries {
		if now.After(e.expiresAt) {
			expired = append(expired, e)
		} else {
			q.entries[n] = e
			n++
		}
	}
	q.entries = q.entries[:n]
	q.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	count := 0
	for _, e := range expired {
		for _, path := range e.paths {
			if err := os.Remove(path); err == nil {
				count++
			} else if !os.IsNotExist(err) {
				slog.Debug("Expiry queue: failed to remove file", "path", path, "error", err)
			}
		}
		if e.onExpiry != nil {
			e.onExpiry()
		}
	}
	if count > 0 {
		slog.Info("Expiry queue swept", "deleted", count)
	}
}

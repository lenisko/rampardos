package services

import (
	"container/heap"
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// OwnedThreshold is the TTL at or above which a path is considered
// "owned by CacheCleaner" — too long to track in the expiry queue.
// An Add at this TTL drops any existing short-TTL entry and marks
// the path as owned, blocking future short-TTL inserts until
// CacheCleaner age-evicts the file and calls Unown.
//
// Callers must only use OwnedThreshold for paths in folders that
// have a CacheCleaner configured (maxAge + clearDelay set). If the
// cleaner is absent, Unown is never called and the owned set grows
// for the process lifetime.
const OwnedThreshold = 24 * time.Hour

// ExpiryQueue tracks files that should be deleted after a TTL.
//
// Internally it uses a min-heap ordered by expiresAt (so sweep
// pops only expired entries without scanning the full set) plus a
// map keyed by path for O(1) lookup on Add/extend.
//
// Add is extend-only: a shorter TTL never shortens an existing
// entry. A TTL >= OwnedThreshold drops the entry entirely and
// marks the path as owned by CacheCleaner.
type ExpiryQueue struct {
	mu    sync.Mutex
	h     expiryHeap
	byKey map[string]*expiryItem
	owned map[string]struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

type expiryItem struct {
	path      string
	expiresAt time.Time
	index     int // managed by container/heap
}

// GlobalExpiryQueue is the shared instance.
var GlobalExpiryQueue *ExpiryQueue

// InitExpiryQueue creates the global queue and starts the sweeper.
func InitExpiryQueue(sweepInterval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &ExpiryQueue{
		byKey: make(map[string]*expiryItem),
		owned: make(map[string]struct{}),
		ctx:   ctx,
		cancel: cancel,
	}
	heap.Init(&q.h)
	GlobalExpiryQueue = q
	go q.sweepLoop(sweepInterval)
}

// Add schedules one or more paths for deletion after ttl. Semantics:
//
//   - If ttl >= OwnedThreshold, any existing short-TTL entry for the
//     path is dropped and the path is marked "owned by CacheCleaner."
//     Future short-TTL Adds for the same path are no-ops until Unown.
//   - Otherwise, extend-only: if the path already has an entry whose
//     expiresAt is later than now+ttl, the call is a no-op. A shorter
//     TTL never shortens an existing scheduled deletion.
//   - If the path is in the owned set, the call is a no-op regardless
//     of TTL (CacheCleaner owns its lifecycle).
func (q *ExpiryQueue) Add(ttl time.Duration, paths ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	for _, p := range paths {
		if ttl >= OwnedThreshold {
			if item, ok := q.byKey[p]; ok {
				heap.Remove(&q.h, item.index)
				delete(q.byKey, p)
			}
			q.owned[p] = struct{}{}
			continue
		}
		if _, ok := q.owned[p]; ok {
			continue
		}
		newExp := now.Add(ttl)
		if item, ok := q.byKey[p]; ok {
			if !newExp.After(item.expiresAt) {
				continue
			}
			item.expiresAt = newExp
			heap.Fix(&q.h, item.index)
			continue
		}
		item := &expiryItem{path: p, expiresAt: newExp}
		heap.Push(&q.h, item)
		q.byKey[p] = item
	}
}

// Unown removes paths from the owned set. Called by CacheCleaner
// when a file is age-evicted so a future request can re-enter the
// expiry-queue lifecycle.
func (q *ExpiryQueue) Unown(paths ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range paths {
		delete(q.owned, p)
	}
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
	var expired []*expiryItem
	for q.h.Len() > 0 && !q.h[0].expiresAt.After(now) {
		item := heap.Pop(&q.h).(*expiryItem)
		delete(q.byKey, item.path)
		expired = append(expired, item)
	}
	q.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	count := 0
	for _, item := range expired {
		if err := os.Remove(item.path); err == nil {
			count++
		} else if !os.IsNotExist(err) {
			slog.Debug("Expiry queue: failed to remove file", "path", item.path, "error", err)
		}
	}
	if count > 0 {
		slog.Info("Expiry queue swept", "deleted", count)
	}
}

// --- min-heap implementation for container/heap ---

type expiryHeap []*expiryItem

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }

func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *expiryHeap) Push(x any) {
	item := x.(*expiryItem)
	item.index = len(*h)
	*h = append(*h, item)
}

func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*h = old[:n-1]
	return item
}

//go:build mln_ffi

package renderer

import (
	"context"
	"testing"
	"time"
)

// TestSetStyleAllReachesEveryWorkerExactlyOnce guards the reload fan-out.
//
// Reloads previously went to the shared work-stealing queue, which cannot
// guarantee one-per-worker: a worker that finished a reload returned to the
// queue and could take a second, leaving a worker that was mid-render at
// broadcast time on the old style. That left a worker serving a stale style
// after a dataset reload, and it gets more likely as pool size shrinks.
//
// The workers here are not started (no loop(), so no FFI/EGL init); the test
// drains the per-worker broadcast channels itself to assert addressing.
func TestSetStyleAllReachesEveryWorkerExactlyOnce(t *testing.T) {
	const workers = 4

	p := &goStylePool{
		cfg:  goStylePoolConfig{styleID: "s", scaleLabel: "1", poolSize: workers},
		cmds: make(chan goWorkerCommand),
	}
	for i := 0; i < workers; i++ {
		p.workers = append(p.workers, &goWorker{
			pool:      p,
			broadcast: make(chan goWorkerCommand, 1),
		})
	}

	// Stand in for the worker loops: each replies to exactly one command
	// taken from its OWN channel, and records how many it saw.
	seen := make([]int, workers)
	done := make(chan struct{})
	for i, w := range p.workers {
		go func(i int, w *goWorker) {
			for {
				select {
				case cmd := <-w.broadcast:
					seen[i]++
					cmd.reply <- goWorkerResult{}
				case <-done:
					return
				}
			}
		}(i, w)
	}
	defer close(done)

	// Nothing must be delivered via the shared queue, which is what the bug
	// relied on. A receive here means the fan-out regressed.
	shared := make(chan struct{}, 1)
	go func() {
		select {
		case <-p.cmds:
			shared <- struct{}{}
		case <-done:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.setStyleAll(ctx, "file:///style.json"); err != nil {
		t.Fatalf("setStyleAll: %v", err)
	}

	for i, n := range seen {
		if n != 1 {
			t.Errorf("worker %d received %d reloads, want exactly 1", i, n)
		}
	}
	select {
	case <-shared:
		t.Error("reload was delivered via the shared queue; fan-out must address workers directly")
	default:
	}
}

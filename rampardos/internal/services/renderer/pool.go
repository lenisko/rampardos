package renderer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lenisko/rampardos/internal/services"
)

// stylePoolConfig configures a single per-style worker pool.
type stylePoolConfig struct {
	styleID          string
	poolSize         int
	workerLifetime   int
	handshakeTimeout time.Duration

	// spawn creates a fresh worker. Injected so tests can use fake
	// workers; production wiring passes a closure that builds a
	// proper `node render-worker.js ...` invocation.
	spawn func() (*worker, error)
}

// stylePool manages workerCount workers for a single style. All
// workers share the same idle channel; dispatch blocks until one is
// available.
type stylePool struct {
	cfg stylePoolConfig

	idle    chan *worker
	mu      sync.Mutex // guards closed, lastPID
	closed  bool
	lastPID int
}

func newStylePool(cfg stylePoolConfig) (*stylePool, error) {
	if cfg.poolSize <= 0 {
		return nil, fmt.Errorf("renderer: pool size must be > 0")
	}
	if cfg.workerLifetime <= 0 {
		cfg.workerLifetime = 500
	}
	if cfg.handshakeTimeout <= 0 {
		cfg.handshakeTimeout = 10 * time.Second
	}

	p := &stylePool{
		cfg:  cfg,
		idle: make(chan *worker, cfg.poolSize),
	}

	// Pre-warm: spawn all workers up front.
	for i := 0; i < cfg.poolSize; i++ {
		w, err := cfg.spawn()
		if err != nil {
			p.close()
			return nil, fmt.Errorf("renderer: pre-warm worker %d: %w", i, err)
		}
		p.idle <- w
	}
	return p, nil
}

// dispatch acquires an idle worker, sends the request, and returns
// the result. If the worker has reached its render lifetime, it is
// killed and replaced before being returned to the idle queue.
func (p *stylePool) dispatch(ctx context.Context, requestJSON []byte) ([]byte, error) {
	acquireStart := time.Now()
	select {
	case w := <-p.idle:
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordRendererPoolAcquire(p.cfg.styleID, time.Since(acquireStart).Seconds(), len(p.idle))
		}
		p.mu.Lock()
		p.lastPID = w.handshake.PID
		p.mu.Unlock()

		payload, err := w.dispatch(ctx, requestJSON)

		// Decide whether to return this worker to the pool, recycle
		// it, or drop it.
		replace := false
		replaceReason := ""
		if err != nil {
			// Any error kills the worker — safer than trying to
			// recover, and the pool refills the slot.
			replace = true
			replaceReason = services.WorkerReplacementError
		} else if int(w.renders.Load()) >= p.cfg.workerLifetime {
			replace = true
			replaceReason = services.WorkerReplacementLifetime
		}

		if replace {
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.RecordRendererWorkerReplacement(p.cfg.styleID, replaceReason)
			}
			w.kill()
			// Best-effort spawn replacement; if it fails we still
			// return the error to the caller but log asynchronously.
			go p.replaceWorker()
		} else {
			p.releaseWorker(w)
		}

		return payload, err

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *stylePool) releaseWorker(w *worker) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		w.kill()
		return
	}
	p.idle <- w
}

func (p *stylePool) replaceWorker() {
	w, err := p.cfg.spawn()
	if err != nil {
		// Log via stderr (same channel worker stderr uses). Pool is
		// now short one worker until next successful spawn; this is
		// acceptable — dispatch still blocks waiting for any idle
		// worker, and callers' context deadlines bound the wait.
		fmt.Fprintf(osStderr, "renderer: failed to replace worker for style %q: %v\n", p.cfg.styleID, err)
		return
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		w.kill()
		return
	}
	p.idle <- w
}

func (p *stylePool) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	// Drain idle workers.
	for {
		select {
		case w := <-p.idle:
			w.kill()
		default:
			return
		}
	}
}

// lastWorkerPIDForTest exposes the pid of the most recently dispatched-to
// worker for tests only.
func (p *stylePool) lastWorkerPIDForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastPID
}

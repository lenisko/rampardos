package renderer

import (
	"testing"
	"time"
)

// newTestPool builds a pool whose workers are inert: startWorker is stubbed,
// so no EGL context, runtime or map is created. That isolates the grow/shrink
// policy from the FFI, which is what these tests are about.
func newTestPool(min, max int) *goStylePool {
	p := &goStylePool{
		cfg: goStylePoolConfig{
			styleID:     "s",
			scaleLabel:  "1",
			poolSize:    max,
			minPoolSize: min,
			idleTTL:     time.Hour, // reap is driven manually
		},
		cmds:       make(chan goWorkerCommand),
		reaperDone: make(chan struct{}),
	}
	p.startWorker = func(startupErrs chan error) *goWorker {
		if startupErrs != nil {
			startupErrs <- nil // report ready immediately
		}
		return &goWorker{pool: p, startupErrs: startupErrs, broadcast: make(chan goWorkerCommand, 1)}
	}
	for i := 0; i < min; i++ {
		p.startWorkerLocked(nil)
	}
	return p
}

func (p *goStylePool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}

func TestPoolGrowsToCeilingThenStops(t *testing.T) {
	p := newTestPool(1, 3)
	if got := p.size(); got != 1 {
		t.Fatalf("initial size = %d, want floor 1", got)
	}

	for i := 0; i < 5; i++ {
		p.grow()
	}
	if got := p.size(); got != 3 {
		t.Errorf("size after repeated growth = %d, want ceiling 3", got)
	}
}

func TestPoolShrinksOneAtATimeDownToFloor(t *testing.T) {
	p := newTestPool(1, 4)
	p.grow()
	p.grow()
	p.grow()
	if got := p.size(); got != 4 {
		t.Fatalf("size after growth = %d, want 4", got)
	}

	// grow() marks the pool saturated, so the first tick is a no-op: a pool
	// that was busy during the interval keeps its capacity.
	if p.reapOnce() {
		t.Error("reaped during an interval that saw saturation; should defer")
	}
	if got := p.size(); got != 4 {
		t.Fatalf("size after deferred reap = %d, want 4", got)
	}

	// Subsequent quiet ticks retire one worker each, stopping at the floor.
	for want := 3; want >= 1; want-- {
		if !p.reapOnce() {
			t.Fatalf("expected a reap down to %d", want)
		}
		if got := p.size(); got != want {
			t.Fatalf("size = %d, want %d", got, want)
		}
	}
	if p.reapOnce() {
		t.Error("reaped below the floor")
	}
	if got := p.size(); got != 1 {
		t.Errorf("final size = %d, want floor 1", got)
	}
}

func TestRetiredWorkerReceivesShutdown(t *testing.T) {
	p := newTestPool(1, 2)
	p.grow()
	p.mu.Lock()
	victim := p.workers[len(p.workers)-1]
	p.mu.Unlock()

	p.saturated = false
	if !p.reapOnce() {
		t.Fatal("expected a reap")
	}
	select {
	case cmd := <-victim.broadcast:
		if cmd.kind != goWorkerCmdShutdown {
			t.Errorf("victim got kind %v, want shutdown", cmd.kind)
		}
	default:
		t.Error("retired worker was not sent a shutdown command")
	}
}

// The high-water mark is the whole point of pool_workers_max: the plain
// gauge is sampled at scrape time, so a pool that grew during a burst and
// decayed before the next scrape would leave no evidence of the peak.
func TestPoolHighWaterSurvivesShrink(t *testing.T) {
	p := newTestPool(1, 4)
	p.grow()
	p.grow()
	p.grow()
	p.reportSize()

	p.mu.Lock()
	peak := p.highWater
	p.mu.Unlock()
	if peak != 4 {
		t.Fatalf("high water after growth = %d, want 4", peak)
	}

	// Decay all the way back to the floor.
	p.saturated = false
	for p.reapOnce() {
		p.saturated = false
	}
	if got := p.size(); got != 1 {
		t.Fatalf("size after full decay = %d, want floor 1", got)
	}

	p.mu.Lock()
	peak = p.highWater
	p.mu.Unlock()
	if peak != 4 {
		t.Errorf("high water after shrink = %d, want it to stay at the peak 4", peak)
	}
}

// A worker that abandons a still-image request and cannot settle it can
// never render again: the FFI clears its pending flag only from the
// completion callback, so every later RequestStillImage returns
// INVALID_STATE. Such a worker deregisters itself, and the pool must drop
// it from the roster so grow() can replace it — otherwise the pool would
// believe it still had capacity it does not have.
func TestDeregisterDropsWorkerAndAllowsRegrowth(t *testing.T) {
	p := newTestPool(1, 2)
	p.grow()
	if got := p.size(); got != 2 {
		t.Fatalf("size after growth = %d, want 2", got)
	}

	p.mu.Lock()
	victim := p.workers[1]
	p.mu.Unlock()

	p.deregister(victim)
	if got := p.size(); got != 1 {
		t.Fatalf("size after deregister = %d, want 1", got)
	}
	p.mu.Lock()
	for _, w := range p.workers {
		if w == victim {
			t.Error("deregistered worker is still on the roster")
		}
	}
	p.mu.Unlock()

	// The freed slot must be reusable, or a pool would shrink permanently
	// each time a worker was retired this way.
	p.grow()
	if got := p.size(); got != 2 {
		t.Errorf("size after regrowth = %d, want 2", got)
	}
}

func TestPoolAtCeilingDoesNotGrowButRecordsSaturation(t *testing.T) {
	p := newTestPool(2, 2)
	p.grow()
	if got := p.size(); got != 2 {
		t.Errorf("size = %d, want 2 (already at ceiling)", got)
	}
	p.mu.Lock()
	saturated := p.saturated
	p.mu.Unlock()
	if !saturated {
		t.Error("saturation must be recorded even when the pool cannot grow, so the reaper holds capacity")
	}
}

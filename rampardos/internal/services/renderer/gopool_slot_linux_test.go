package renderer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lenisko/rampardos/internal/services"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
)

// TestGlobalSlotHeldUntilRenderEnds guards RENDERER_POOL_SIZE. A worker
// keeps rendering after its request's context is cancelled; freeing the
// global slot when the caller gave up let more renders run at once than
// the cap allows.
func TestGlobalSlotHeldUntilRenderEnds(t *testing.T) {
	p := newTestPool(1, 1)
	t.Cleanup(p.close)

	// Stand in for a worker: take one render and hold it until finish,
	// then end it the way the real loop does.
	finish := make(chan struct{})
	taken := make(chan struct{})
	ended := make(chan struct{})
	t.Cleanup(func() { <-ended })
	go func() {
		defer close(ended)
		cmd := <-p.cmds
		close(taken)
		<-finish
		if cmd.render.release != nil {
			cmd.render.release()
		}
		cmd.reply <- goWorkerResult{err: errors.New("done")}
	}()

	r := &GoPoolRenderer{
		sem:   semaphore.NewWeighted(1),
		pools: map[string]*goStylePool{poolKey("s", 1): p},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := r.RenderViewportImage(ctx, ViewportRequest{StyleID: "s", Width: 1, Height: 1, Scale: 1})
		errs <- err
	}()
	<-taken
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled render: got %v, want context.Canceled", err)
	}

	if r.sem.TryAcquire(1) {
		t.Fatal("global slot freed while the worker was still rendering")
	}
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for !r.sem.TryAcquire(1) {
		if time.Now().After(deadline) {
			t.Fatal("global slot not freed after the render ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// globalInFlightMax reads the rampardos_renderer_global_in_flight_max
// gauge.
func globalInFlightMax(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "rampardos_renderer_global_in_flight_max" {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("rampardos_renderer_global_in_flight_max not registered")
	return 0
}

// The in-flight high-water mark is read against the cap to size
// RENDERER_POOL_SIZE. Freeing the slot before lowering the count let the
// next render count itself first, so prod showed 9 renders in flight
// under a cap of 8.
func TestGlobalInFlightNeverExceedsCapacity(t *testing.T) {
	services.InitMetrics()
	if peak := globalInFlightMax(t); peak > 1 {
		t.Skipf("an earlier test already raised the in-flight peak to %v", peak)
	}
	r := &GoPoolRenderer{sem: semaphore.NewWeighted(1)}

	const iterations = 100000
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				if err := r.acquireGlobal(context.Background()); err != nil {
					t.Error(err)
					return
				}
				r.releaseGlobal()
			}
		}()
	}
	wg.Wait()
	if peak := globalInFlightMax(t); peak > 1 {
		t.Fatalf("in-flight peak reached %v under a capacity of 1", peak)
	}
}

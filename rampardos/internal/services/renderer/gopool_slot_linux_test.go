package renderer

import (
	"context"
	"errors"
	"testing"
	"time"

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
	go func() {
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

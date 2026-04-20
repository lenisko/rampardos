package handlers

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/utils"
)

// raceFakePNG returns a valid 1x1 PNG for tests.
func raceFakePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// raceTestStaticMap is a stable StaticMap whose BasePath is
// deterministic across goroutines in a test.
func raceTestStaticMap() models.StaticMap {
	return models.StaticMap{
		Style:     "local",
		Latitude:  51.5,
		Longitude: 0.0,
		Zoom:      14,
		Width:     256,
		Height:    256,
	}
}

// raceTestHandler builds a minimal StaticMapHandler whose base
// generators route through renderFn. The "local" style means
// generateBaseStaticMap takes the tiles branch at integer zoom;
// renderFn is wired into both tile and API hooks for safety.
func raceTestHandler(t *testing.T, renderFn func(ctx context.Context, sm models.StaticMap, basePath string) (image.Image, error)) (*StaticMapHandler, func()) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	h := &StaticMapHandler{
		stylesController:  stubStylesController{ext: nil},
		sphericalMercator: utils.NewSphericalMercator(),
	}
	h.generateBaseStaticMapFromTilesFn = func(ctx context.Context, sm models.StaticMap, _ *models.Style) (image.Image, error) {
		return renderFn(ctx, sm, sm.BasePath())
	}
	h.generateBaseStaticMapFromAPIFn = func(ctx context.Context, sm models.StaticMap) (image.Image, error) {
		return renderFn(ctx, sm, sm.BasePath())
	}
	h.logExternalViewportApproxFn = func(sm models.StaticMap) {}

	return h, func() { _ = os.Chdir(oldDir) }
}

// TestEnsureBaseDeletedBetweenCallsDoesNotError locks in the stale-
// index-removed invariant: deleting basePath externally between
// calls must trigger a fresh render, not a 500 or a skipped render.
func TestEnsureBaseDeletedBetweenCallsDoesNotError(t *testing.T) {
	var renders atomic.Int32
	renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) (image.Image, error) {
		renders.Add(1)
		img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
		img.Set(0, 0, color.RGBA{A: 255})
		if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(basePath, raceFakePNG(t), 0o644); err != nil {
			return nil, err
		}
		return img, nil
	}
	h, cleanup := raceTestHandler(t, renderFn)
	defer cleanup()

	sm := raceTestStaticMap()
	basePath := sm.BasePath()
	ctx := context.Background()

	if _, err := h.ensureBase(ctx, sm, basePath, false); err != nil {
		t.Fatalf("first ensureBase: %v", err)
	}
	if got := renders.Load(); got != 1 {
		t.Fatalf("expected 1 render after first call, got %d", got)
	}
	if _, err := os.Stat(basePath); err != nil {
		t.Fatalf("expected persisted base: %v", err)
	}

	if err := os.Remove(basePath); err != nil {
		t.Fatalf("remove base: %v", err)
	}

	if _, err := h.ensureBase(ctx, sm, basePath, false); err != nil {
		t.Fatalf("ensureBase after delete: %v", err)
	}
	if got := renders.Load(); got != 2 {
		t.Fatalf("expected 2 renders after re-trigger, got %d", got)
	}
}

// TestGenerateStaticMapSingleflightSurvivesLeaderCancel locks in the
// invariant that when multiple callers join the outer sfg for the same
// final path, cancelling the leader's context must not abort the
// generation for concurrent waiters that are still live. Before the
// fix the sfg.Do callback used the leader's ctx verbatim, so a client
// disconnect on the leader cancelled the shared render function and
// returned ctx.Err() to every waiter.
func TestGenerateStaticMapSingleflightSurvivesLeaderCancel(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)

	renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) (image.Image, error) {
		entered <- struct{}{}
		select {
		case <-gate:
			img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
			img.Set(0, 0, color.RGBA{A: 255})
			if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(basePath, raceFakePNG(t), 0o644); err != nil {
				return nil, err
			}
			return img, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h, cleanup := raceTestHandler(t, renderFn)
	defer cleanup()

	sm := raceTestStaticMap()

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	followerCtx, cancelFollower := context.WithCancel(context.Background())
	defer cancelFollower()

	leaderErr := make(chan error, 1)
	followerErr := make(chan error, 1)

	go func() { _, err := h.GenerateStaticMap(leaderCtx, sm, false); leaderErr <- err }()

	// Wait for the leader to enter the renderFn so we know it has the
	// sfg slot. The follower arriving after this is guaranteed to
	// attach to the same sfg group.
	<-entered

	go func() { _, err := h.GenerateStaticMap(followerCtx, sm, false); followerErr <- err }()

	// Give the follower time to subscribe to the sfg group.
	time.Sleep(20 * time.Millisecond)

	// Simulate leader client disconnect.
	cancelLeader()

	// Release the blocked render. With the fix the render fn's ctx is
	// detached from leaderCtx, so the render completes normally. Without
	// the fix the render returned ctx.Err() the moment cancelLeader ran
	// and the follower inherits that error from sfg.
	close(gate)

	select {
	case err := <-followerErr:
		if err != nil {
			t.Fatalf("follower aborted due to leader cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not complete within timeout")
	}

	<-leaderErr
}

// TestNoCacheForcesFreshRenderButPopulatesLRU locks in the
// post-poracle-migration nocache semantics: nocache=true bypasses
// the composite LRU read (forces a fresh render) but still writes
// the result back so other callers benefit.
func TestNoCacheForcesFreshRenderButPopulatesLRU(t *testing.T) {
	prev := services.GlobalCompositeImageCache
	services.GlobalCompositeImageCache = services.NewCompositeImageCache(16)
	t.Cleanup(func() { services.GlobalCompositeImageCache = prev })

	var renders atomic.Int32
	renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) (image.Image, error) {
		renders.Add(1)
		img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
		img.Set(0, 0, color.RGBA{A: 255})
		return img, nil
	}
	h, cleanup := raceTestHandler(t, renderFn)
	defer cleanup()

	sm := raceTestStaticMap()
	ctx := context.Background()

	// First call: nothing cached, must render.
	if _, err := h.GenerateStaticMap(ctx, sm, false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := renders.Load(); got != 1 {
		t.Fatalf("after first call: want 1 render, got %d", got)
	}

	// Second call without nocache: must hit the LRU, not render again.
	if _, err := h.GenerateStaticMap(ctx, sm, false); err != nil {
		t.Fatalf("cached call: %v", err)
	}
	if got := renders.Load(); got != 1 {
		t.Fatalf("after cached call: LRU miss, want 1 render, got %d", got)
	}

	// Third call with nocache=true: must re-render despite hot LRU.
	if _, err := h.GenerateStaticMap(ctx, sm, true); err != nil {
		t.Fatalf("nocache call: %v", err)
	}
	if got := renders.Load(); got != 2 {
		t.Fatalf("after nocache call: want 2 renders, got %d", got)
	}

	// Fourth call without nocache: nocache must have populated the LRU,
	// so this hits cache — no new render.
	if _, err := h.GenerateStaticMap(ctx, sm, false); err != nil {
		t.Fatalf("post-nocache cached call: %v", err)
	}
	if got := renders.Load(); got != 2 {
		t.Fatalf("after post-nocache cached call: want 2 renders, got %d", got)
	}
}

// TestEnsureBaseSingleflightDedupesSiblings locks in the baseSfg
// singleflight behaviour: N concurrent callers for the same basePath
// trigger exactly one render.
//
// Determinism in two stages:
//  1. Each goroutine signals `started` immediately before calling
//     ensureBase. Draining N signals proves all N are live.
//  2. The render fn blocks on `gate` so the leader cannot release
//     its singleflight slot before followers arrive. We read one
//     `reached` signal to confirm the leader is inside the render
//     fn, then briefly yield for the N-1 followers to suspend
//     inside baseSfg.Do (the only call between "started" and the
//     render fn for them), then release the gate.
//
// singleflight.Group exposes no "pending count" so the final yield
// is necessarily wall-clock, but the start barrier shrinks the
// window it has to cover from "all goroutines must run" to just
// "N-1 goroutines must enter Do" — a handful of instructions.
func TestEnsureBaseSingleflightDedupesSiblings(t *testing.T) {
	const N = 8
	var renders atomic.Int32
	started := make(chan struct{}, N)
	reached := make(chan struct{}, N+1)
	gate := make(chan struct{})

	renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) (image.Image, error) {
		renders.Add(1)
		reached <- struct{}{}
		<-gate
		img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
		img.Set(0, 0, color.RGBA{A: 255})
		if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(basePath, raceFakePNG(t), 0o644); err != nil {
			return nil, err
		}
		return img, nil
	}
	h, cleanup := raceTestHandler(t, renderFn)
	defer cleanup()

	sm := raceTestStaticMap()
	basePath := sm.BasePath()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			if _, err := h.ensureBase(ctx, sm, basePath, false); err != nil {
				t.Errorf("ensureBase: %v", err)
			}
		}()
	}

	for i := 0; i < N; i++ {
		<-started
	}
	<-reached
	time.Sleep(10 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := renders.Load(); got != 1 {
		t.Fatalf("expected 1 base render for %d concurrent callers, got %d", N, got)
	}
	if extra := len(reached); extra != 0 {
		t.Fatalf("expected no extra render fn entries, got %d", extra)
	}
}

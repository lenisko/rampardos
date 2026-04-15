package handlers

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
)

// raceTestHandler returns a handler wired up so that base generation
// is stubbed (no renderer, no tile handler) and returns fakePNG bytes.
// The atomic counter records how many times the fake renderer ran.
func raceTestHandler(t *testing.T) (*StaticMapHandler, *atomic.Int32, func()) {
	t.Helper()
	h, cleanup := newTestStaticMapHandler(t)
	var renders atomic.Int32
	h.stylesController = stubStylesController{ext: nil}
	h.generateBaseStaticMapFromTilesFn = func(ctx context.Context, sm models.StaticMap, _ *models.Style) ([]byte, error) {
		renders.Add(1)
		return fakePNG(t), nil
	}
	h.generateBaseStaticMapFromAPIFn = func(ctx context.Context, sm models.StaticMap) ([]byte, error) {
		renders.Add(1)
		return fakePNG(t), nil
	}
	h.logExternalViewportApproxFn = func(sm models.StaticMap) {}
	return h, &renders, cleanup
}

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

// TestBaseDeletedBetweenCallsDoesNotError verifies that when the base
// file is deleted externally between requests, the next call
// regenerates it — no stale cache-index false positives.
func TestBaseDeletedBetweenCallsDoesNotError(t *testing.T) {
	h, renders, cleanup := raceTestHandler(t)
	defer cleanup()

	sm := raceTestStaticMap()
	basePath := sm.BasePath()
	ctx := context.Background()

	// First call renders + persists.
	b1, err := h.loadOrRenderBaseBytes(ctx, sm, basePath)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(b1) == 0 {
		t.Fatal("first load returned empty bytes")
	}
	if renders.Load() != 1 {
		t.Fatalf("expected 1 render after first call, got %d", renders.Load())
	}
	if _, err := os.Stat(basePath); err != nil {
		t.Fatalf("expected persisted base at %s: %v", basePath, err)
	}

	// External deletion of the shared base.
	if err := os.Remove(basePath); err != nil {
		t.Fatalf("remove base: %v", err)
	}

	// Second call must not fail and must re-render.
	b2, err := h.loadOrRenderBaseBytes(ctx, sm, basePath)
	if err != nil {
		t.Fatalf("second load after base delete: %v", err)
	}
	if len(b2) == 0 {
		t.Fatal("second load returned empty bytes")
	}
	if renders.Load() != 2 {
		t.Fatalf("expected 2 renders after re-trigger, got %d", renders.Load())
	}
}

// TestConcurrentSiblingBaseSingleflight verifies that N concurrent
// requests for the same basePath render it exactly once.
func TestConcurrentSiblingBaseSingleflight(t *testing.T) {
	h, renders, cleanup := raceTestHandler(t)
	defer cleanup()

	sm := raceTestStaticMap()
	basePath := sm.BasePath()
	ctx := context.Background()

	const N = 8
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.loadOrRenderBaseBytes(ctx, sm, basePath); err != nil {
				t.Errorf("load: %v", err)
			}
		}()
	}
	wg.Wait()

	got := renders.Load()
	if got != 1 {
		t.Fatalf("expected 1 base render for %d concurrent callers, got %d", N, got)
	}
}

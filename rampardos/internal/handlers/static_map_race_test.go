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
func raceTestHandler(t *testing.T, renderFn func(ctx context.Context, sm models.StaticMap, basePath string) error) (*StaticMapHandler, func()) {
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
	h.generateBaseStaticMapFromTilesFn = func(ctx context.Context, sm models.StaticMap, basePath string, _ *models.Style) error {
		return renderFn(ctx, sm, basePath)
	}
	h.generateBaseStaticMapFromAPIFn = renderFn
	h.logExternalViewportApproxFn = func(sm models.StaticMap) {}

	return h, func() { _ = os.Chdir(oldDir) }
}

// TestEnsureBaseDeletedBetweenCallsDoesNotError locks in the stale-
// index-removed invariant: deleting basePath externally between
// calls must trigger a fresh render, not a 500 or a skipped render.
func TestEnsureBaseDeletedBetweenCallsDoesNotError(t *testing.T) {
	var renders atomic.Int32
	renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) error {
		renders.Add(1)
		if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(basePath, raceFakePNG(t), 0o644)
	}
	h, cleanup := raceTestHandler(t, renderFn)
	defer cleanup()

	sm := raceTestStaticMap()
	basePath := sm.BasePath()
	ctx := context.Background()

	if err := h.ensureBase(ctx, sm, basePath); err != nil {
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

	if err := h.ensureBase(ctx, sm, basePath); err != nil {
		t.Fatalf("ensureBase after delete: %v", err)
	}
	if got := renders.Load(); got != 2 {
		t.Fatalf("expected 2 renders after re-trigger, got %d", got)
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

	renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) error {
		renders.Add(1)
		reached <- struct{}{}
		<-gate
		if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(basePath, raceFakePNG(t), 0o644)
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
			if err := h.ensureBase(ctx, sm, basePath); err != nil {
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

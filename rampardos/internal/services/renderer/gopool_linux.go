// Package renderer's in-process implementation, driving maplibre-native
// through its C ABI. Linux-only: it needs EGL and links
// libmaplibre-native-c.so. The _linux suffix is the whole constraint —
// non-Linux builds get the stub in gopool_other.go.
//
// Per-pool internals are plain worker goroutines: with the binding's
// core-worker driver a dedicated (private) EGL session owns its context
// on a native worker thread, so no host call here is thread-affine —
// runtime, map, session, and operation handles are all goroutine-safe.
// A worker goroutine exists only as a concurrency lane owning one
// runtime+map+session for its lifetime.
package renderer

import (
	"context"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lenisko/rampardos/internal/fileutil"
	"github.com/lenisko/rampardos/internal/services"
	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go/maplibre"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/sync/semaphore"
)

// Ensure GoPoolRenderer satisfies the Renderer interface.
var _ Renderer = (*GoPoolRenderer)(nil)

// GoPoolRenderer is the in-process Renderer implementation backed by
// the upstream maplibre-native Go binding driving Mesa OpenGL via EGL
// surfaceless. One worker goroutine per pool slot; each session's
// private EGL context lives on a native core worker, so workers are
// plain goroutines. Same outer shape as the previous implementations:
// lazy (style, scale)-keyed pools, two-level concurrency
// (process-global semaphore × per-pool slot count), encode at the
// renderer boundary.
type GoPoolRenderer struct {
	cfg Config
	sem *semaphore.Weighted

	// store is the process-wide tile provider backing every worker's
	// resource-provider callback: one SQLite pool + one tile LRU instead
	// of a native MBTilesFileSource per worker runtime. Nil when
	// RENDERER_TILE_PROVIDER=off (falls back to mbtiles:// sources).
	store *TileStore

	// ambientCacheBytes is each worker runtime's mbgl ambient cache
	// budget; negative leaves mbgl's default.
	ambientCacheBytes int64

	mu    sync.RWMutex
	pools map[string]*goStylePool
}

func NewGoPoolRenderer(cfg Config) (Renderer, error) {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = runtime.GOMAXPROCS(0)
	}
	if cfg.StylePoolSize <= 0 {
		cfg.StylePoolSize = cfg.PoolSize
	}
	if cfg.StylePoolMin <= 0 || cfg.StylePoolMin > cfg.StylePoolSize {
		cfg.StylePoolMin = 1
	}
	if cfg.StylePoolIdleTTL <= 0 {
		cfg.StylePoolIdleTTL = 10 * time.Minute
	}
	if cfg.RenderTimeout <= 0 {
		cfg.RenderTimeout = 15 * time.Second
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 30 * time.Second
	}

	// Catch a wrong-variant FFI at process start, not at first render.
	if !maplibre.SupportedRenderBackendMask().Has(maplibre.RenderBackendFlagOpenGl) {
		return nil, fmt.Errorf("renderer: linked FFI does not advertise OpenGL backend; check Dockerfile MISE_ENV variant (expected linux-<arch>-egl)")
	}

	if _, err := cfg.DiscoverStyles(); err != nil {
		return nil, fmt.Errorf("renderer: discover styles: %w", err)
	}

	// The shared tile provider is on by default; RENDERER_TILE_PROVIDER=off
	// is the kill switch back to the native mbtiles source, and a store
	// that fails to open degrades the same way rather than blocking boot.
	var store *TileStore
	if os.Getenv("RENDERER_TILE_PROVIDER") != "off" {
		cacheMB := 64
		if v, err := strconv.Atoi(os.Getenv("RENDERER_TILE_CACHE_MB")); err == nil && v > 0 {
			cacheMB = v
		}
		st, err := newTileStore(cfg.MbtilesFile, int64(cacheMB)<<20)
		if err != nil {
			slog.Warn("Tile provider disabled: store open failed; falling back to native mbtiles source", "error", err)
		} else if tj, err := st.TileJSON(); err != nil {
			slog.Warn("Tile provider disabled: dataset metadata unusable; falling back to native mbtiles source", "error", err)
			st.Close()
		} else {
			cfg.TileJSON = tj
			store = st
			slog.Info("Tile provider active", "mbtiles", cfg.MbtilesFile, "cacheMB", cacheMB, "maxzoom", tj["maxzoom"])
		}
	}

	// mbgl keeps an ambient cache per runtime: each worker holds its own
	// copy of every tile it fetched and, before using one, asks the
	// provider to revalidate it, which the provider answers by resending
	// the tile. With the provider active its LRU already serves every
	// worker, so the ambient cache costs a duplicate per worker and a
	// second delivery for nothing. RENDERER_AMBIENT_CACHE_MB re-enables
	// it for a local style that pulls remote sources. Negative leaves
	// mbgl's default (the native mbtiles path bypasses the cache anyway).
	ambientCacheBytes := int64(-1)
	if store != nil {
		ambientCacheBytes = 0
	}
	if v, err := strconv.Atoi(os.Getenv("RENDERER_AMBIENT_CACHE_MB")); err == nil && v >= 0 {
		ambientCacheBytes = int64(v) << 20
	}

	r := &GoPoolRenderer{
		cfg:               cfg,
		sem:               semaphore.NewWeighted(int64(cfg.PoolSize)),
		store:             store,
		ambientCacheBytes: ambientCacheBytes,
		pools:             make(map[string]*goStylePool),
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.SetRendererGlobalCapacity(cfg.PoolSize)
	}
	return r, nil
}

func (r *GoPoolRenderer) acquireGlobal(ctx context.Context) error {
	start := time.Now()
	if err := r.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererGlobalAcquire(time.Since(start).Seconds())
	}
	return nil
}

func (r *GoPoolRenderer) releaseGlobal() {
	r.sem.Release(1)
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.DecRendererGlobalInFlight()
	}
}

func (r *GoPoolRenderer) Render(ctx context.Context, req Request) ([]byte, error) {
	tileSize := NormalizeTileSize(req.TileSize)
	if req.Z == 0 && tileSize < TileSizePx {
		return r.renderDownscaledWorldTile(ctx, req, tileSize)
	}
	vp := TileToViewport(req.Z, req.X, req.Y, req.Scale, tileSize)
	vp.StyleID = req.StyleID
	vp.Format = req.Format
	img, err := r.renderViewportImage(ctx, vp, false)
	if err != nil {
		return nil, err
	}
	return encodeRGBAImage(img, req.Format)
}

// renderDownscaledWorldTile serves a sub-512 tile at z=0. The small
// tile path renders at zoom z-1, but MapLibre clamps zoom to its
// minimum of 0, so the frame would show only part of the world.
// Render the 512 world tile instead and resample it to size.
func (r *GoPoolRenderer) renderDownscaledWorldTile(ctx context.Context, req Request, tileSize int) ([]byte, error) {
	vp := TileToViewport(0, 0, 0, req.Scale, TileSizePx)
	vp.StyleID = req.StyleID
	vp.Format = req.Format
	full, err := r.renderViewportImage(ctx, vp, false)
	if err != nil {
		return nil, err
	}
	px := tileSize * int(vp.Scale)
	small := image.NewNRGBA(image.Rect(0, 0, px, px))
	xdraw.CatmullRom.Scale(small, small.Bounds(), full, full.Bounds(), xdraw.Src, nil)
	return encodeRGBAImage(small, req.Format)
}

func (r *GoPoolRenderer) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
	img, err := r.RenderViewportImage(ctx, req)
	if err != nil {
		return nil, err
	}
	return encodeRGBAImage(img, req.Format)
}

func (r *GoPoolRenderer) RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error) {
	return r.renderViewportImage(ctx, req, true)
}

func (r *GoPoolRenderer) renderViewportImage(ctx context.Context, req ViewportRequest, applyZoomAdj bool) (*image.NRGBA, error) {
	scale := max(int(req.Scale), 1)

	pool, err := r.getOrCreatePool(req.StyleID, req.Scale)
	if err != nil {
		return nil, err
	}

	if err := r.acquireGlobal(ctx); err != nil {
		return nil, err
	}
	defer r.releaseGlobal()

	adjusted := req
	if applyZoomAdj {
		adjusted.Zoom = req.Zoom - pool.cfg.viewportZoomAdj
		slog.Debug("renderer viewport dispatch",
			"style", req.StyleID,
			"scale", req.Scale,
			"callerZoom", req.Zoom,
			"maplibreZoom", adjusted.Zoom,
			"zoomAdj", pool.cfg.viewportZoomAdj,
		)
	}

	return pool.dispatch(ctx, adjusted, scale)
}

func (r *GoPoolRenderer) loadPool(id string, ratio int) (*goStylePool, error) {
	styleFile := filepath.Join(r.cfg.StylesDir, id, "style.json")
	raw, err := os.ReadFile(styleFile)
	if err != nil {
		return nil, fmt.Errorf("read style.json: %w", err)
	}

	prepared, err := PrepareStyle(id, raw, r.cfg)
	if err != nil {
		return nil, fmt.Errorf("prepare style: %w", err)
	}

	preparedPath := filepath.Join(r.cfg.StylesDir, id, "style.prepared.json")
	if err := fileutil.AtomicWriteFile(preparedPath, prepared, 0o644); err != nil {
		return nil, fmt.Errorf("write prepared style: %w", err)
	}

	zoomAdj := styleZoomOffset(raw)

	cfg := goStylePoolConfig{
		store:             r.store,
		ambientCacheBytes: r.ambientCacheBytes,
		styleID:           id,
		scaleLabel:        strconv.Itoa(ratio),
		viewportZoomAdj:   zoomAdj,
		poolSize:          r.cfg.StylePoolSize,
		minPoolSize:       r.cfg.StylePoolMin,
		idleTTL:           r.cfg.StylePoolIdleTTL,
		ratio:             ratio,
		styleURL:          "file://" + preparedPath,
		startupTimeout:    r.cfg.StartupTimeout,
		renderTimeout:     r.cfg.RenderTimeout,
	}
	return newGoStylePool(cfg)
}

func (r *GoPoolRenderer) getOrCreatePool(styleID string, scale uint8) (*goStylePool, error) {
	if scale < 1 {
		scale = 1
	}
	key := poolKey(styleID, scale)

	r.mu.RLock()
	pool, ok := r.pools[key]
	r.mu.RUnlock()
	if ok {
		return pool, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if pool, ok := r.pools[key]; ok {
		return pool, nil
	}

	stylePath := filepath.Join(r.cfg.StylesDir, styleID, "style.json")
	if _, err := os.Stat(stylePath); err != nil {
		return nil, fmt.Errorf("renderer: unknown style %q (no style.json at %s)", styleID, stylePath)
	}

	ratio := int(scale)
	slog.Info("Creating Go renderer pool on first use", "style", styleID, "ratio", ratio, "stylePoolSize", r.cfg.StylePoolSize, "globalRenderCap", r.cfg.PoolSize)
	pool, err := r.loadPool(styleID, ratio)
	if err != nil {
		return nil, fmt.Errorf("renderer: create pool %q ratio=%d: %w", styleID, ratio, err)
	}
	r.pools[key] = pool
	return pool, nil
}

// ReloadStyles broadcasts a reload command to every worker in every
// pool. Workers process commands serially, so reload queues behind any
// in-flight render on each worker. Under sustained load this briefly
// serialises reload — same as the previous implementation's setStyleAll
// drain pattern.
func (r *GoPoolRenderer) ReloadStyles(ctx context.Context) error {
	// Dataset activate/combine retargets the mbtiles symlink; follow it
	// before re-preparing styles so the inline TileJSON matches the new
	// dataset's zoom range and the tile cache drops stale blobs.
	if r.store != nil {
		if err := r.store.Reopen(); err != nil {
			slog.Error("Reload: tile store reopen failed; keeping previous dataset handle", "error", err)
		} else if tj, err := r.store.TileJSON(); err != nil {
			slog.Error("Reload: dataset metadata unusable; keeping previous TileJSON", "error", err)
		} else {
			r.mu.Lock()
			r.cfg.TileJSON = tj
			r.mu.Unlock()
		}
	}

	r.mu.RLock()
	keys := make([]string, 0, len(r.pools))
	for key := range r.pools {
		keys = append(keys, key)
	}
	r.mu.RUnlock()

	for _, key := range keys {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.RLock()
		pool, ok := r.pools[key]
		r.mu.RUnlock()
		if !ok {
			continue
		}

		styleID, ratio := parsePoolKey(key)

		styleFile := filepath.Join(r.cfg.StylesDir, styleID, "style.json")
		raw, err := os.ReadFile(styleFile)
		if err != nil {
			slog.Error("Reload: read style.json failed; pool kept on previous style", "style", styleID, "ratio", ratio, "error", err)
			continue
		}
		prepared, err := PrepareStyle(styleID, raw, r.cfg)
		if err != nil {
			slog.Error("Reload: prepare style failed; pool kept on previous style", "style", styleID, "ratio", ratio, "error", err)
			continue
		}
		preparedPath := filepath.Join(r.cfg.StylesDir, styleID, "style.prepared.json")
		if err := fileutil.AtomicWriteFile(preparedPath, prepared, 0o644); err != nil {
			slog.Error("Reload: write prepared style failed; pool kept on previous style", "style", styleID, "ratio", ratio, "error", err)
			continue
		}

		styleURL := "file://" + preparedPath
		zoomAdj := styleZoomOffset(raw)

		if err := pool.setStyleAll(ctx, styleURL); err != nil {
			slog.Error("Reload: setStyleAll failed; pool may be partially swapped", "style", styleID, "ratio", ratio, "error", err)
			continue
		}
		pool.mu.Lock()
		pool.cfg.viewportZoomAdj = zoomAdj
		pool.cfg.styleURL = styleURL
		pool.mu.Unlock()
	}
	return nil
}

func (r *GoPoolRenderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pool := range r.pools {
		pool.close()
	}
	r.pools = nil
	if r.store != nil {
		r.store.Close()
	}
	return nil
}

// goStylePoolConfig captures the per-pool parameters. Pool members are
// scale-segregated; ratio is therefore baked in at pool construction.
type goStylePoolConfig struct {
	store *TileStore // shared tile provider; nil = native mbtiles path
	// ambientCacheBytes is the runtime's mbgl ambient cache budget;
	// negative leaves mbgl's default.
	ambientCacheBytes int64
	styleID           string
	scaleLabel        string
	viewportZoomAdj   float64
	poolSize          int // ceiling: most workers this pool may hold
	minPoolSize       int // floor: workers held even when idle
	idleTTL           time.Duration
	settleTimeout     time.Duration
	ratio             int
	styleURL          string // "file://<preparedPath>"
	startupTimeout    time.Duration
	renderTimeout     time.Duration
}

// goStylePool owns N worker goroutines (one per pool slot). Each worker
// is OS-thread-pinned and holds its own RuntimeHandle + MapHandle +
// RenderSessionHandle for its lifetime; the eglDisplay is process-wide.
type goStylePool struct {
	cfg goStylePoolConfig

	cmds chan goWorkerCommand
	wg   sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	workers []*goWorker // guarded by mu; addressed by broadcast operations

	// saturated records whether any dispatch since the last reaper tick
	// found every worker busy. Growth is immediate on demand; shrink only
	// happens after a whole tick with no saturation, so a pool ramps up
	// fast and decays slowly rather than thrashing around the boundary.
	saturated  bool
	highWater  int // most workers this pool has held; guarded by mu
	reaperDone chan struct{}

	// startWorker is the worker constructor, swappable in tests so the
	// grow/shrink policy can be exercised without standing up real EGL
	// contexts. Nil means use the real one.
	startWorker func(startupErrs chan error) *goWorker

	// resourceErrs holds the glyph and sprite failures already logged,
	// so a resource that fails on every render warns once per pool (a
	// style reload builds a new pool and warns again).
	resourceErrs sync.Map
}

type goWorkerCmdKind int

const (
	goWorkerCmdRender goWorkerCmdKind = iota
	goWorkerCmdReload
	goWorkerCmdShutdown
)

type goWorkerCommand struct {
	kind   goWorkerCmdKind
	ctx    context.Context
	render *goRenderRequest // non-nil iff kind == goWorkerCmdRender
	reload string           // styleURL, non-empty iff kind == goWorkerCmdReload
	reply  chan goWorkerResult
}

type goRenderRequest struct {
	vp    ViewportRequest
	scale int
}

type goWorkerResult struct {
	img *image.NRGBA
	err error
}

// newGoStylePool spawns poolSize worker goroutines, each of which locks
// an OS thread, initialises its EGL context + Runtime + Map + Session,
// loads the style, and enters the command loop. The factory blocks
// until every worker has either signalled readiness or failed; any
// worker startup failure tears the whole pool down and returns the
// first error.
func newGoStylePool(cfg goStylePoolConfig) (*goStylePool, error) {
	if cfg.poolSize <= 0 {
		return nil, fmt.Errorf("renderer: pool size must be > 0")
	}
	if cfg.startupTimeout <= 0 {
		cfg.startupTimeout = 30 * time.Second
	}
	if cfg.renderTimeout <= 0 {
		cfg.renderTimeout = 15 * time.Second
	}
	if cfg.minPoolSize <= 0 || cfg.minPoolSize > cfg.poolSize {
		cfg.minPoolSize = 1
	}
	if cfg.idleTTL <= 0 {
		cfg.idleTTL = 10 * time.Minute
	}
	if cfg.settleTimeout <= 0 {
		cfg.settleTimeout = 5 * time.Second
	}

	p := &goStylePool{
		cfg:        cfg,
		cmds:       make(chan goWorkerCommand),
		reaperDone: make(chan struct{}),
	}

	// Spawn the floor only. Pools are created per (style, scale), so with
	// several styles a static count over-provisions every cold pool while
	// still capping the hot one; starting small and growing on demand lets
	// a busy combination reach the ceiling without charging idle ones for
	// it. Matters most on small hosts, where each worker is a full mbgl
	// map, runtime and EGL context.
	startupErrs := make(chan error, cfg.minPoolSize)
	p.workers = make([]*goWorker, 0, cfg.poolSize)
	for i := 0; i < cfg.minPoolSize; i++ {
		p.startWorkerLocked(startupErrs)
	}

	// Wait for all workers to report startup status. First failure
	// triggers a teardown of any already-started workers.
	deadline := time.NewTimer(cfg.startupTimeout)
	defer deadline.Stop()
	var firstErr error
	collected := 0
	for collected < cfg.minPoolSize {
		select {
		case err := <-startupErrs:
			collected++
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-deadline.C:
			firstErr = fmt.Errorf("renderer: pool startup timeout after %s (workers ready %d/%d)", cfg.startupTimeout, collected, cfg.minPoolSize)
			collected = cfg.minPoolSize // break the loop
		}
	}
	if firstErr != nil {
		// Close the cmd channel so any already-running workers exit
		// their loops and tear down their state. Wait for them.
		close(p.cmds)
		p.wg.Wait()
		return nil, fmt.Errorf("renderer: build pool for style %q ratio=%d: %w", cfg.styleID, cfg.ratio, firstErr)
	}

	go p.reap()
	p.reportSize()
	return p, nil
}

// startWorkerLocked constructs, registers and starts one worker. Callers
// hold p.mu (or are in construction, before the pool is published).
// startupErrs may be nil for workers grown after construction: nobody is
// collecting, so failures are logged by the caller of grow() instead.
func (p *goStylePool) startWorkerLocked(startupErrs chan error) *goWorker {
	if p.startWorker != nil {
		w := p.startWorker(startupErrs)
		p.workers = append(p.workers, w)
		return w
	}
	w := &goWorker{
		pool:        p,
		startupErrs: startupErrs,
		broadcast:   make(chan goWorkerCommand, 1),
	}
	p.workers = append(p.workers, w)
	p.wg.Add(1)
	go w.loop()
	return w
}

// grow adds one worker if the pool is below its ceiling. Called when a
// dispatch found every worker busy. The new worker's init (EGL context,
// runtime, map, style load) happens on its own goroutine, so the request
// that triggered growth is not made to wait for it — it queues on the
// shared channel as usual and whichever worker frees up first serves it.
func (p *goStylePool) grow() {
	p.mu.Lock()
	p.saturated = true
	if p.closed || len(p.workers) >= p.cfg.poolSize {
		p.mu.Unlock()
		return
	}
	errs := make(chan error, 1)
	p.startWorkerLocked(errs)
	size := len(p.workers)
	p.mu.Unlock()

	slog.Debug("renderer pool growing", "style", p.cfg.styleID, "scale", p.cfg.scaleLabel, "workers", size, "max", p.cfg.poolSize)
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererPoolGrow(p.cfg.styleID, p.cfg.scaleLabel)
	}
	p.reportSize()

	go func() {
		if err := <-errs; err != nil {
			// The worker has already torn itself down and exited; drop it
			// from the roster so the count stays honest and a later
			// dispatch can retry growth.
			p.mu.Lock()
			for i, w := range p.workers {
				if w.startupErrs == errs {
					p.workers = append(p.workers[:i], p.workers[i+1:]...)
					break
				}
			}
			p.mu.Unlock()
			p.reportSize()
			slog.Warn("renderer pool growth failed", "style", p.cfg.styleID, "scale", p.cfg.scaleLabel, "error", err)
		}
	}()
}

// reap retires one worker per idle interval, down to the floor. One at a
// time so a pool that just went quiet keeps some capacity for a while
// rather than collapsing to the floor the moment traffic pauses.
func (p *goStylePool) reap() {
	ticker := time.NewTicker(p.cfg.idleTTL)
	defer ticker.Stop()
	for {
		select {
		case <-p.reaperDone:
			return
		case <-ticker.C:
			p.reapOnce()
		}
	}
}

// deregister drops a worker that is exiting on its own (currently only a
// poisoned one) so the roster stays honest and grow() can replace it.
func (p *goStylePool) deregister(w *goWorker) {
	p.mu.Lock()
	for i, candidate := range p.workers {
		if candidate == w {
			p.workers = append(p.workers[:i], p.workers[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererWorkerReplacement(p.cfg.styleID, p.cfg.scaleLabel, services.WorkerReplacementError)
	}
	p.reportSize()
}

// reapOnce retires at most one worker. Returns true if it did.
func (p *goStylePool) reapOnce() bool {
	p.mu.Lock()
	busy := p.saturated
	p.saturated = false
	if p.closed || busy || len(p.workers) <= p.cfg.minPoolSize {
		p.mu.Unlock()
		return false
	}
	victim := p.workers[len(p.workers)-1]
	p.workers = p.workers[:len(p.workers)-1]
	size := len(p.workers)
	p.mu.Unlock()

	// Buffered channel, so this never blocks on a worker that is mid-render;
	// it picks the shutdown up when it returns to the loop.
	victim.broadcast <- goWorkerCommand{kind: goWorkerCmdShutdown}
	slog.Debug("renderer pool shrinking", "style", p.cfg.styleID, "scale", p.cfg.scaleLabel, "workers", size, "min", p.cfg.minPoolSize)
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererPoolShrink(p.cfg.styleID, p.cfg.scaleLabel)
	}
	p.reportSize()
	return true
}

// reportSize publishes the pool's current size and its high-water mark.
// Both are read under the same lock so the peak can never lag behind a
// size it was derived from.
func (p *goStylePool) reportSize() {
	p.mu.Lock()
	n := len(p.workers)
	if n > p.highWater {
		p.highWater = n
	}
	peak := p.highWater
	p.mu.Unlock()

	if services.GlobalMetrics != nil {
		services.GlobalMetrics.SetRendererPoolWorkers(p.cfg.styleID, p.cfg.scaleLabel, n, peak)
	}
}

// dispatch sends a render command to whichever worker is ready (or
// blocks until one is). The unbuffered cmds channel + per-worker
// single-flight processing means we never queue more renders than
// workers — backpressure is natural.
func (p *goStylePool) dispatch(ctx context.Context, vp ViewportRequest, scale int) (*image.NRGBA, error) {
	acquireStart := time.Now()
	reply := make(chan goWorkerResult, 1)
	cmd := goWorkerCommand{
		kind:   goWorkerCmdRender,
		ctx:    ctx,
		render: &goRenderRequest{vp: vp, scale: scale},
		reply:  reply,
	}

	// Try to hand the work to an already-idle worker. A failed non-blocking
	// send means every worker is busy, which is the only signal available
	// for demand: the shared channel is unbuffered, so a successful send is
	// exactly "someone was waiting for work".
	select {
	case p.cmds <- cmd:
		// picked up immediately
	default:
		p.grow()
		select {
		case p.cmds <- cmd:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererPoolAcquire(p.cfg.styleID, p.cfg.scaleLabel, time.Since(acquireStart).Seconds())
	}

	select {
	case res := <-reply:
		return res.img, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// setStyleAll broadcasts a reload command to every worker. Workers
// process commands serially so reload queues behind in-flight renders.
// Returns the first error from any worker.
func (p *goStylePool) setStyleAll(ctx context.Context, styleURL string) error {
	p.mu.Lock()
	workers := make([]*goWorker, len(p.workers))
	copy(workers, p.workers)
	p.mu.Unlock()

	// Address each worker's own channel rather than the shared queue, so
	// every worker reloads exactly once. Post to all of them first, then
	// collect: the channels are buffered, so a worker busy rendering picks
	// its reload up when it returns to the loop instead of blocking us.
	replies := make([]chan goWorkerResult, len(workers))
	for i, w := range workers {
		replies[i] = make(chan goWorkerResult, 1)
		cmd := goWorkerCommand{
			kind:   goWorkerCmdReload,
			ctx:    ctx,
			reload: styleURL,
			reply:  replies[i],
		}
		select {
		case w.broadcast <- cmd:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var firstErr error
	for i := range workers {
		select {
		case res := <-replies[i]:
			if res.err != nil && firstErr == nil {
				firstErr = res.err
			}
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return firstErr
		}
	}
	return firstErr
}

func (p *goStylePool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.reaperDone)
	close(p.cmds)
	p.wg.Wait()
}

// goWorker is one pool slot. Owns one Runtime, one Map, one
// RenderSession whose core worker owns the EGL context, and the
// display+config it borrows. Plain goroutine — nothing here is
// thread-affine: the native session worker does all graphics work, and
// every async call returns a Future safe to await from any goroutine.
// The still drives its own frame demands; the worker only waits for it,
// woken by the session's FrameWake and the runtime event handler.
type goWorker struct {
	pool        *goStylePool
	startupErrs chan<- error

	// broadcast receives commands addressed to THIS worker specifically,
	// as opposed to pool.cmds which is a shared work-stealing queue. Style
	// reloads must reach every worker exactly once, which a shared channel
	// cannot guarantee: a worker that finishes one reload returns to the
	// queue and can take a second, leaving a worker that was mid-render at
	// broadcast time with a stale style. Buffered so setStyleAll can post
	// to every worker without deadlocking against workers that are busy.
	broadcast chan goWorkerCommand

	rt   *maplibre.RuntimeHandle
	m    *maplibre.MapHandle
	sess *maplibre.RenderSessionHandle
	egl  *eglDisplay

	// wake is fed by the session's FrameWake callback, which native
	// invokes (from any thread, calls may coalesce) whenever a frame result
	// becomes drainable, and by the event handler on a style failure.
	// Buffered-1 + non-blocking send turns bursts into one pending token.
	wake chan struct{}

	// mapErr is fed by the runtime event handler with the map's render
	// errors. Buffered-1 + non-blocking send keeps the first one until
	// the await consumes it.
	mapErr chan error

	// styleFailed is the latest style load failure the event handler
	// saw. It is state, not a signal: mbgl fails only the still pending
	// when a style fails to load, and a still requested afterwards waits
	// for a style that never loads, so every render must refuse while it
	// stands. It belongs to the current style only when reported after
	// that style's command committed (generation > styleGen); a new style
	// command clears it by moving styleGen past it.
	styleFailed atomic.Pointer[styleFailure]
	// resourceFailed is the latest glyph or sprite load failure. Such a
	// failure fails the pending still with a bare native error, so the
	// await adds this to say which resource. It is style state: a failed
	// sprite fails every later still without a new event. It belongs to
	// the current style when reported at or after the generation that
	// style's command committed at (generation >= styleGen: a sprite that
	// fails before the map publishes another snapshot carries the commit
	// generation itself, while an earlier style's failure is always
	// below it). A fast load can fail before setStyle returns, so it is
	// never cleared there. A failed glyph range is never requested
	// again, so a later still that needs it never completes: the worker
	// retires after the render it failed, and its replacement's fresh map
	// fails fast again.
	resourceFailed atomic.Pointer[resourceFailure]
	// styleGen is the map generation at which the current style command
	// committed, and styleURL the URL it set. Worker goroutine only.
	styleGen uint64
	styleURL string

	curW     uint32
	curH     uint32
	curScale float64

	// poisoned is set when an abandoned still-image request could not be
	// settled, or a glyph range failed (see resourceFailed). The map
	// refuses a second still while one is pending, so the pool retires
	// the worker rather than dispatching to a map that may never start
	// or finish another render.
	poisoned bool
}

func (w *goWorker) loop() {
	defer w.pool.wg.Done()

	if err := w.init(); err != nil {
		w.startupErrs <- err
		w.cleanup()
		return
	}
	w.startupErrs <- nil
	defer w.cleanup()

	for {
		var cmd goWorkerCommand
		select {
		case c, ok := <-w.pool.cmds:
			if !ok {
				return // pool closed
			}
			cmd = c
		case c := <-w.broadcast:
			cmd = c
		}

		switch cmd.kind {
		case goWorkerCmdShutdown:
			// Retired by the reaper. Deferred cleanup releases the
			// session's core worker, map, runtime and their mbgl
			// caches — the whole point of shrinking.
			if cmd.reply != nil {
				cmd.reply <- goWorkerResult{}
			}
			return
		case goWorkerCmdRender:
			img, err := w.renderOne(cmd.ctx, cmd.render.vp, cmd.render.scale)
			cmd.reply <- goWorkerResult{img: img, err: err}
			if w.poisoned {
				w.pool.deregister(w)
				return
			}
		case goWorkerCmdReload:
			// The load itself is not awaited: renders observe a failure
			// through styleFailed.
			cmd.reply <- goWorkerResult{err: w.setStyle(cmd.ctx, cmd.reload)}
		}
	}
}

func (w *goWorker) init() error {
	var err error
	w.egl, err = newEGLDisplay()
	if err != nil {
		return err
	}

	w.wake = make(chan struct{}, 1)
	w.mapErr = make(chan error, 1)

	w.rt, err = maplibre.NewRuntime(maplibre.RuntimeOptions{})
	if err != nil {
		return fmt.Errorf("renderer: new runtime: %w", err)
	}

	startupCtx, cancel := context.WithTimeout(context.Background(), w.pool.cfg.startupTimeout)
	defer cancel()

	// Events are dropped until a handler is set, so it is set before the
	// map exists. The handler runs on the runtime's native thread and
	// must not block: it only records style failures and forwards
	// render errors.
	f, err := w.rt.SetEventHandler(maplibre.RuntimeEventHandler{Callback: w.handleEvents})
	if err != nil {
		return fmt.Errorf("renderer: set event handler: %w", err)
	}
	if _, err := f.Await(startupCtx); err != nil {
		return fmt.Errorf("renderer: set event handler: %w", err)
	}

	if w.pool.cfg.ambientCacheBytes >= 0 {
		f, err := w.rt.SetMaximumAmbientCacheSize(uint64(w.pool.cfg.ambientCacheBytes))
		if err != nil {
			return fmt.Errorf("renderer: set ambient cache size: %w", err)
		}
		if _, err := f.Await(startupCtx); err != nil {
			return fmt.Errorf("renderer: set ambient cache size: %w", err)
		}
	}

	if w.pool.cfg.store != nil {
		f, err := w.rt.SetResourceProvider(w.pool.cfg.store.provider())
		if err != nil {
			return fmt.Errorf("renderer: set resource provider: %w", err)
		}
		if _, err := f.Await(startupCtx); err != nil {
			return fmt.Errorf("renderer: set resource provider: %w", err)
		}
	}

	// MapModeStatic is the render-once mode; the executor disables the
	// animation tick and aligns with our request/reply pattern. The event
	// mask carries the two failures no still reports by itself: a style
	// that fails to load fails only the still pending at that moment (see
	// styleFailed), and a render error ends no still. Glyph and sprite
	// failures do fail the still, but with no reason; their events name
	// the resource (see resourceFailed).
	w.curW, w.curH, w.curScale = 256, 256, float64(w.pool.cfg.ratio)
	events := maplibre.RuntimeEventFlagMapLoadingFailed | maplibre.RuntimeEventFlagMapRenderError |
		maplibre.RuntimeEventFlagMapGlyphsError | maplibre.RuntimeEventFlagMapSpriteError
	m, ready, err := w.rt.CreateMap(maplibre.MapOptions{
		InitialExtent: maplibre.LogicalExtent{
			Width:       w.curW,
			Height:      w.curH,
			ScaleFactor: w.curScale,
		},
		MapMode:   maplibre.MapModeStatic,
		EventMask: &events,
	})
	if err != nil {
		return fmt.Errorf("renderer: new map: %w", err)
	}
	w.m = m
	if _, err := ready.Await(startupCtx); err != nil {
		return fmt.Errorf("renderer: new map: %w", err)
	}

	// Core-worker driver with a dedicated (private) EGL context: the
	// native session worker creates and owns the context from our
	// display+config and needs no host graphics service. The FrameWake
	// callback only schedules (non-blocking channel send) per the wake
	// contract; the await drains the frame queue itself. A private target
	// grants one texture slot whatever the ring depth asks for.
	wakeCallback := func() {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
	sess, attached, err := w.m.AttachOpenGlOwnedTexture(
		maplibre.OpenGlOwnedTextureDescriptor{
			Extent: maplibre.LogicalExtent{
				Width:       w.curW,
				Height:      w.curH,
				ScaleFactor: w.curScale,
			},
			Context: w.egl.descriptor(),
		},
		maplibre.RenderSessionAttachOptions{
			Driver:    maplibre.RenderDriverKindCoreWorker,
			FrameWake: maplibre.Wake{Callback: wakeCallback},
		},
	)
	if err != nil {
		return fmt.Errorf("renderer: attach OpenGL owned texture render target: %w", err)
	}
	w.sess = sess
	if _, err := attached.Await(startupCtx); err != nil {
		return fmt.Errorf("renderer: attach: %w", err)
	}

	caps, err := w.sess.Capabilities()
	if err != nil {
		return fmt.Errorf("renderer: session capabilities: %w", err)
	}
	if caps.Flags&maplibre.RenderSessionCapabilityFlagReadback == 0 {
		return fmt.Errorf("renderer: attached session does not grant readback; cannot render stills")
	}

	// Style loading proceeds on the native executor; a failure reaches
	// renders through styleFailed. Worker reports ready once the map
	// adopts the URL.
	return w.setStyle(startupCtx, w.pool.cfg.styleURL)
}

// styleFailure is a style load failure and the map generation that
// reported it.
type styleFailure struct {
	generation uint64
	message    string
}

// styleErr reports whether the current style failed to load.
func (w *goWorker) styleErr() error {
	if f := w.styleFailed.Load(); f != nil && f.generation > w.styleGen {
		return fmt.Errorf("renderer: style failed to load: %s", f.message)
	}
	return nil
}

// setStyle submits a style URL and waits for the command to commit (a
// command that fails or is cancelled returns an error from Await). It
// commits once the map adopts the URL, before the style loads, so the
// wait is brief; a command that did not apply would otherwise leave the
// worker on its old style with nothing reported. Recording the commit
// generation retires any failure of the previous style.
func (w *goWorker) setStyle(ctx context.Context, url string) error {
	f, err := w.m.SetStyleUrl(url)
	if err != nil {
		return fmt.Errorf("renderer: set style url: %w", err)
	}
	cc, err := f.Await(ctx)
	if err != nil {
		return fmt.Errorf("renderer: set style url: %w", err)
	}
	w.styleGen = cc.Generation
	w.styleURL = url
	if serviceTrace {
		fmt.Fprintf(os.Stderr, "TRACE style committed gen=%d\n", cc.Generation)
	}
	return nil
}

func (w *goWorker) cleanup() {
	settleCtx, cancel := context.WithTimeout(context.Background(), w.pool.cfg.settleTimeout)
	defer cancel()
	if w.sess != nil {
		// Detach completes on the core worker; Abandon is the
		// no-graphics-work fallback that quarantines resources instead.
		detached := false
		if f, err := w.sess.Detach(); err == nil {
			if _, err := f.Await(settleCtx); err == nil {
				detached = true
			}
		}
		if !detached {
			_, _ = w.sess.Abandon()
		}
		if f, err := w.sess.Close(); err == nil {
			_, _ = f.Await(settleCtx)
		}
	}
	if w.m != nil {
		if f, err := w.m.Close(); err == nil {
			_, _ = f.Await(settleCtx)
		}
	}
	if w.rt != nil {
		if f, err := w.rt.Close(); err == nil {
			_, _ = f.Await(settleCtx)
		}
	}
	// w.egl is the process-wide display; it outlives every worker.
}

// renderOne is the per-worker hot path. Resize only if extent changed
// (saves the texture-realloc when consecutive requests share size).
func (w *goWorker) renderOne(ctx context.Context, vp ViewportRequest, scale int) (*image.NRGBA, error) {
	if vp.Width <= 0 || vp.Height <= 0 {
		return nil, fmt.Errorf("renderer: invalid dimensions %dx%d", vp.Width, vp.Height)
	}
	// A still on a style that failed to load would never complete.
	if err := w.styleErr(); err != nil {
		return nil, err
	}

	wantW := uint32(vp.Width)
	wantH := uint32(vp.Height)
	wantScale := float64(scale)
	var resize *maplibre.Future[maplibre.CommandCompletion]
	if wantW != w.curW || wantH != w.curH || wantScale != w.curScale {
		// Resize applies the extent and updates the map viewport. On a
		// static map the driver applies it without waiting for a map
		// update, and the next still renders at the new extent; session
		// operations are ordered ahead of the still's frames, so it is
		// started here and its completion checked after the still.
		f, err := w.sess.Resize(maplibre.LogicalExtent{
			Width:       wantW,
			Height:      wantH,
			ScaleFactor: wantScale,
		})
		if err != nil {
			return nil, fmt.Errorf("renderer: resize: %w", err)
		}
		resize = f
		// An accepted resize applies whatever happens to this render, so
		// track the extent from here. Leaving the old one recorded when a
		// render fails would let the next request at the old size skip its
		// resize and read back a frame at the size the session now has.
		w.curW, w.curH, w.curScale = wantW, wantH, wantScale
	}

	zoom, bearing, pitch := vp.Zoom, vp.Bearing, vp.Pitch
	camera, err := w.m.UpdateCamera(maplibre.CameraUpdate{
		Mode: maplibre.CameraUpdateModeJump,
		Camera: maplibre.CameraOptions{
			Center:  &maplibre.LatLng{Latitude: vp.Latitude, Longitude: vp.Longitude},
			Zoom:    &zoom,
			Bearing: &bearing,
			Pitch:   &pitch,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("renderer: camera: %w", err)
	}

	still, err := w.m.RequestStillImage()
	if err != nil {
		return nil, fmt.Errorf("renderer: request still image: %w", err)
	}
	if err := w.awaitStill(ctx, still, w.pool.cfg.renderTimeout); err != nil {
		// The request may still be pending, and the map refuses a second
		// still while one is. Let it settle (bounded); poison the worker
		// if it does not.
		w.settleAbandonedStill(still)
		return nil, err
	}

	// The still rendered the camera and any resize, but their completions
	// can land a beat behind it. A command that did not commit means the
	// image may show the previous view or size, so it fails the render
	// rather than serving it.
	if err := w.awaitCommitted(ctx, camera, "camera"); err != nil {
		return nil, err
	}
	if resize != nil {
		if err := w.awaitCommitted(ctx, resize, "resize"); err != nil {
			// Force the next render to resize: the session's extent is
			// no longer known.
			w.curW, w.curH = 0, 0
			return nil, err
		}
	}

	physW := vp.Width * scale
	physH := vp.Height * scale

	rb, err := w.sess.ReadTexture()
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, w.pool.cfg.renderTimeout)
	tr, err := rb.Await(readCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	if int(tr.Info.Width) != physW || int(tr.Info.Height) != physH {
		w.curW, w.curH = 0, 0 // the target's extent is not what we recorded
		return nil, fmt.Errorf("renderer: size mismatch: got %dx%d, want %dx%d", tr.Info.Width, tr.Info.Height, physW, physH)
	}
	// Guard the tight unpremultiply loop against a padded stride: it
	// assumes densely packed rows, and a mismatch would index past the
	// destination. Not expected from the RGBA8 readback; fail cleanly
	// rather than panic if an upstream change introduces padding.
	if len(tr.Data) != physW*physH*4 {
		return nil, fmt.Errorf("renderer: readback length mismatch: got %d bytes (stride %d), want %d", len(tr.Data), tr.Info.Stride, physW*physH*4)
	}
	// The binding hands back a Go-owned copy of the pixels, so convert
	// it in place and use it as the image rather than allocating a
	// second frame-sized buffer.
	unpremultiplyRGBA(tr.Data)
	return &image.NRGBA{Pix: tr.Data, Stride: physW * 4, Rect: image.Rect(0, 0, physW, physH)}, nil
}

// awaitCommitted waits briefly for a map or session command the still
// has already rendered, and fails unless it committed: Await returns an
// error for a command that failed or was cancelled.
func (w *goWorker) awaitCommitted(ctx context.Context, f *maplibre.Future[maplibre.CommandCompletion], what string) error {
	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := f.Await(flushCtx); err != nil {
		return fmt.Errorf("renderer: %s: %w", what, err)
	}
	return nil
}

// settleAbandonedStill waits, bounded and off the caller's (typically
// already cancelled) context, for an abandoned still request to end.
// There is no cancel for a pending still and the map refuses a second
// one, but the request submits its own frame demands, so it settles by
// itself once the map has loaded what it shows.
//
// A still on a failed style is the exception. mbgl fails the still
// pending when a style load fails, but the failure event can reach us
// after renderOne checked for it and before the request ran, leaving a
// still that waits for a style that never loads. Loading the style
// again fails it the same way, which fails that still. A worker whose
// still does not settle within the budget is poisoned and retired by the
// pool.
func (w *goWorker) settleAbandonedStill(still *maplibre.Future[struct{}]) {
	select {
	case <-still.Done():
		// Already terminal — it failed, or completed as the render gave
		// up. The map holds no pending still, so the worker is healthy.
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.pool.cfg.settleTimeout)
	defer cancel()
	reloaded := false
	for {
		if !reloaded && w.styleErr() != nil {
			reloaded = true
			if err := w.setStyle(ctx, w.styleURL); err != nil {
				slog.Warn("renderer: reloading a failed style to settle a still failed",
					"style", w.pool.cfg.styleID, "scale", w.pool.cfg.scaleLabel, "error", err)
			}
		}
		select {
		case <-still.Done():
			return
		case <-w.wake: // a frame result or a style failure; recheck
		case <-ctx.Done():
			w.poisoned = true
			slog.Warn("renderer worker retired: abandoned still-image request did not settle",
				"style", w.pool.cfg.styleID, "scale", w.pool.cfg.scaleLabel, "timeout", w.pool.cfg.settleTimeout, "styleError", w.styleErr())
			return
		}
	}
}

// serviceTrace enables per-event stderr tracing of the still await.
// Debug aid; cheap and env-gated.
var serviceTrace = os.Getenv("RENDERER_SERVICE_TRACE") != ""

// handleEvents is the runtime event handler. It runs on the runtime's
// native thread, so it never blocks. A style failure is recorded, then
// the wake is nudged so a parked await sees it. A render error is a
// non-blocking send: one already pending is kept and later ones are
// dropped, since the await fails on the first. The events are the
// binding's copy.
func (w *goWorker) handleEvents(events []maplibre.RuntimeEvent) {
	for _, ev := range events {
		switch ev.Type {
		case maplibre.RuntimeEventTypeMapLoadingFailed:
			w.styleFailed.Store(&styleFailure{generation: ev.Generation, message: ev.Message})
			select {
			case w.wake <- struct{}{}:
			default:
			}
		case maplibre.RuntimeEventTypeMapRenderError:
			select {
			case w.mapErr <- fmt.Errorf("renderer: map render error: %s", ev.Message):
			default:
			}
		case maplibre.RuntimeEventTypeMapGlyphsError, maplibre.RuntimeEventTypeMapSpriteError:
			w.reportResourceError(ev)
		default:
			continue
		}
		if serviceTrace {
			fmt.Fprintf(os.Stderr, "TRACE event type=%d gen=%d msg=%q\n", ev.Type, ev.Generation, ev.Message)
		}
	}
}

type resourceFailure struct {
	generation uint64
	glyphs     bool
	what       string // kind and resource, e.g. "glyphs Noto Sans 0-255"
}

// resourceErr returns the current style's glyph or sprite failure, if
// any. Worker goroutine only (it reads styleGen).
func (w *goWorker) resourceErr() *resourceFailure {
	if r := w.resourceFailed.Load(); r != nil && r.generation >= w.styleGen {
		return r
	}
	return nil
}

// reportResourceError records a glyph or sprite load failure for the
// render's error, counts it, and warns the first time this pool sees
// it. It runs on the runtime's native thread, so the log write is
// handed to a goroutine.
func (w *goWorker) reportResourceError(ev maplibre.RuntimeEvent) {
	kind, detail := "sprite", ""
	switch p := ev.Payload.(type) {
	case maplibre.RuntimeEventPayloadGlyphsVariant:
		kind = "glyphs"
		detail = fmt.Sprintf("%s %d-%d", strings.Join(p.Value.FontStack, ","), p.Value.First, p.Value.Last)
	case maplibre.RuntimeEventPayloadSpriteVariant:
		detail = p.Value.Url
		if detail == "" {
			detail = p.Value.Id
		}
	default:
		if ev.Type == maplibre.RuntimeEventTypeMapGlyphsError {
			kind = "glyphs"
		}
	}
	what := kind + " " + detail
	w.resourceFailed.Store(&resourceFailure{generation: ev.Generation, glyphs: kind == "glyphs", what: what})
	select {
	case w.wake <- struct{}{}:
	default:
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.IncRendererResourceError(w.pool.cfg.styleID, w.pool.cfg.scaleLabel, kind)
	}
	if _, seen := w.pool.resourceErrs.LoadOrStore(what, struct{}{}); seen {
		return
	}
	style, scale, msg := w.pool.cfg.styleID, w.pool.cfg.scaleLabel, ev.Message
	go slog.Warn("renderer: style resource failed to load; renders with this style fail",
		"style", style, "scale", scale, "kind", kind, "resource", detail, "error", msg)
}

// awaitResourceFailure returns the resource failure behind a failed
// still. Its event can trail the Future, so a still that failed without
// one waits briefly for it; any other failure pays only that wait.
func (w *goWorker) awaitResourceFailure() *resourceFailure {
	if r := w.resourceErr(); r != nil {
		return r
	}
	grace := time.NewTimer(100 * time.Millisecond)
	defer grace.Stop()
	for {
		select {
		case <-w.wake:
			if r := w.resourceErr(); r != nil {
				return r
			}
		case <-grace.C:
			return w.resourceErr()
		}
	}
}

// drainFrames drains the session's queued frame results, records them,
// and reports how many arrived. Only still demands render on this
// session, so every result belongs to a still. A TargetNotReady result
// fails the render: the still would wait for a target change that never
// comes.
func (w *goWorker) drainFrames(start time.Time) (int, error) {
	results, err := w.sess.DrainFrameResults()
	if err != nil {
		return 0, fmt.Errorf("renderer: still image: drain frame results: %w", err)
	}
	for _, res := range results {
		if serviceTrace {
			fmt.Fprintf(os.Stderr, "TRACE +%6dus frame disp=%d update=%d\n", time.Since(start).Microseconds(), res.Disposition, res.MapUpdateGeneration)
		}
		w.countFrameResult(res)
		if res.Disposition == maplibre.RenderResultTargetNotReady {
			return len(results), fmt.Errorf("renderer: still image: render target not ready")
		}
	}
	return len(results), nil
}

func (w *goWorker) countFrameResult(res maplibre.RenderFrameResult) {
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.IncRendererStillFrameResult(w.pool.cfg.styleID, w.pool.cfg.scaleLabel, dispositionLabel(res.Disposition))
	}
}

// awaitStill waits for a still-image request to complete. The request
// submits its own frame demands on the session and resubmits after any
// that ends before the image renders, so the host only waits: on the
// still's Future, the FrameWake channel (frame results, drained for
// metrics and to catch an unusable target), and the map-failure
// signals. The readback may follow as soon as the Future completes.
func (w *goWorker) awaitStill(ctx context.Context, still *maplibre.Future[struct{}], budget time.Duration) error {
	start := time.Now()
	timer := time.NewTimer(budget)
	defer timer.Stop()

	// Results queued since the last render belong to an earlier still:
	// usually the completing frame of the previous one, whose result
	// trailed its Future past that render's final drain, or an abandoned
	// still that settled later. They are counted, so the frame-result
	// metric stays whole, but not acted on. A wake latched by them is
	// stale too, and so is a render error: the map renders only for a
	// still, so one left over was raised by an earlier render, which has
	// already failed on it.
	for {
		results, err := w.sess.DrainFrameResults()
		if err != nil {
			return fmt.Errorf("renderer: still image: flush frame results: %w", err)
		}
		if len(results) == 0 {
			break
		}
		for _, res := range results {
			w.countFrameResult(res)
		}
	}
	select {
	case <-w.wake:
	default:
	}
	select {
	case <-w.mapErr:
	default:
	}
	// The flushed wake may have carried a style failure that landed after
	// renderOne checked; nothing would wake the loop for it again.
	if err := w.styleErr(); err != nil {
		return err
	}
	// A glyph failure whose event trailed the render it failed: this map
	// will never finish a still that needs the range.
	if r := w.resourceErr(); r != nil && r.glyphs {
		w.poisoned = true
		return fmt.Errorf("renderer: still image: %s failed to load", r.what)
	}

	frames := 0
	for {
		select {
		case <-still.Done():
			if _, err := still.Await(ctx); err != nil {
				if r := w.awaitResourceFailure(); r != nil {
					w.poisoned = w.poisoned || r.glyphs
					return fmt.Errorf("renderer: still image: %s failed to load: %w", r.what, err)
				}
				return fmt.Errorf("renderer: still image: %w", err)
			}
			// The completing frame's result can trail the Future.
			n, _ := w.drainFrames(start)
			frames += n
			if serviceTrace {
				fmt.Fprintf(os.Stderr, "TRACE +%6dus still COMPLETED frames=%d\n", time.Since(start).Microseconds(), frames)
			}
			return nil
		case <-w.wake:
			n, err := w.drainFrames(start)
			frames += n
			if err != nil {
				return err
			}
			// A style that failed after this still was requested leaves it
			// pending for good: mbgl failed only a still already pending.
			// A still it did fail is done; let that case report it.
			if err := w.styleErr(); err != nil {
				select {
				case <-still.Done():
					continue
				default:
					return err
				}
			}
		case err := <-w.mapErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("renderer: still image timed out after %s", budget)
		}
	}
}

func dispositionLabel(d maplibre.RenderResult) string {
	switch d {
	case maplibre.RenderResultRendered:
		return "rendered"
	case maplibre.RenderResultNoUpdate:
		return "no_update"
	case maplibre.RenderResultTargetNotReady:
		return "target_not_ready"
	case maplibre.RenderResultSuperseded:
		return "superseded"
	default:
		return "other"
	}
}

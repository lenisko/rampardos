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
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/lenisko/rampardos/internal/fileutil"
	"github.com/lenisko/rampardos/internal/services"
	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
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
	backends, err := maplibre.SupportedRenderBackendMask()
	if err != nil {
		return nil, fmt.Errorf("renderer: query render backends: %w", err)
	}
	if !backends.Has(maplibre.RenderBackendFlagOpengl) {
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

	r := &GoPoolRenderer{
		cfg:   cfg,
		sem:   semaphore.NewWeighted(int64(cfg.PoolSize)),
		store: store,
		pools: make(map[string]*goStylePool),
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
	vp := TileToViewport(req.Z, req.X, req.Y, req.Scale)
	vp.StyleID = req.StyleID
	vp.Format = req.Format
	img, err := r.renderViewportImage(ctx, vp, false)
	if err != nil {
		return nil, err
	}
	return encodeRGBAImage(img, req.Format)
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
		store:           r.store,
		styleID:         id,
		scaleLabel:      strconv.Itoa(ratio),
		viewportZoomAdj: zoomAdj,
		poolSize:        r.cfg.StylePoolSize,
		minPoolSize:     r.cfg.StylePoolMin,
		idleTTL:         r.cfg.StylePoolIdleTTL,
		ratio:           ratio,
		styleURL:        "file://" + preparedPath,
		startupTimeout:  r.cfg.StartupTimeout,
		renderTimeout:   r.cfg.RenderTimeout,
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
	store           *TileStore // shared tile provider; nil = native mbtiles path
	styleID         string
	scaleLabel      string
	viewportZoomAdj float64
	poolSize        int // ceiling: most workers this pool may hold
	minPoolSize     int // floor: workers held even when idle
	idleTTL         time.Duration
	settleTimeout   time.Duration
	ratio           int
	styleURL        string // "file://<preparedPath>"
	startupTimeout  time.Duration
	renderTimeout   time.Duration
}

// goStylePool owns N worker goroutines (one per pool slot). Each worker
// is OS-thread-pinned and holds its own RuntimeHandle + MapHandle +
// RenderSessionHandle + eglDisplay for its lifetime.
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
		services.GlobalMetrics.RecordRendererPoolAcquire(p.cfg.styleID, p.cfg.scaleLabel, time.Since(acquireStart).Seconds(), 0)
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
// The session's FrameWake feeds the worker's wake channel, so the
// still await is event-driven rather than tick-polled.
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

	// wake is fed by the session's FrameWake callback: native invokes it
	// (from any thread, calls may coalesce) whenever a frame result
	// becomes drainable. Buffered-1 + non-blocking send turns bursts
	// into one pending token.
	wake chan struct{}

	curW     uint32
	curH     uint32
	curScale float64

	// frameToken numbers frame demands so drained results can be matched
	// to the still image currently being driven.
	frameToken uint64

	// poisoned is set when an abandoned still-image request could not be
	// settled. The map refuses a second still while one is pending, so
	// the pool retires the worker rather than dispatching to a map that
	// may never start another render.
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
			// SetStyleUrl is a command — the executor loads the style
			// in the background and the next render observes completion
			// or failure. Reply on the submission result so reload
			// doesn't block renders; the command's own completion is
			// deliberately not awaited.
			_, err := w.m.SetStyleUrl(cmd.reload)
			cmd.reply <- goWorkerResult{err: err}
		}
	}
}

func (w *goWorker) init() error {
	var err error
	w.egl, err = newEGLDisplay()
	if err != nil {
		return err
	}

	// The wake channel is shared by the runtime's EventWake and the
	// session's FrameWake: the still await parks on it and does not care
	// which side fired — it drains both queues every turn. Created
	// before RuntimeCreate because EventWake is a runtime option.
	w.wake = make(chan struct{}, 1)
	wakeCallback := func() {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}

	opts := maplibre.DefaultRuntimeOptions()
	// Wakes when the runtime event queue becomes nonempty. Update-driven
	// demands depend on this: a MapRenderUpdateAvailable event must end
	// the await's park even when no frame result is in flight.
	opts.EventWake = maplibre.Wake{Callback: wakeCallback}
	w.rt, err = maplibre.RuntimeCreate(opts)
	if err != nil {
		return fmt.Errorf("renderer: new runtime: %w", err)
	}

	startupCtx, cancel := context.WithTimeout(context.Background(), w.pool.cfg.startupTimeout)
	defer cancel()

	if w.pool.cfg.store != nil {
		f, err := w.rt.SetResourceProvider(maplibre.ResourceProvider{Callback: w.pool.cfg.store.Provide})
		if err != nil {
			return fmt.Errorf("renderer: set resource provider: %w", err)
		}
		if _, err := f.Await(startupCtx); err != nil {
			return fmt.Errorf("renderer: set resource provider: %w", err)
		}
	}

	// MapModeStatic is the render-once mode; the executor disables the
	// animation tick and aligns with our request/reply pattern. The event
	// mask carries the two failure events the still await fail-fasts on
	// plus MapRenderUpdateAvailable, which drives demands: the map
	// publishes an update whenever state or queued render-thread work is
	// available, and the await answers each with one frame demand. Still
	// completion arrives via its Future.
	w.curW, w.curH, w.curScale = 256, 256, float64(w.pool.cfg.ratio)
	mapFuture, err := w.rt.MapCreate(maplibre.MapOptions{
		InitialExtent: maplibre.LogicalExtent{
			Width:       w.curW,
			Height:      w.curH,
			ScaleFactor: w.curScale,
		},
		MapMode: maplibre.MapModeStatic,
		EventMask: maplibre.RuntimeEventMaskMapLoadingFailed |
			maplibre.RuntimeEventMaskMapRenderError |
			maplibre.RuntimeEventMaskMapRenderUpdateAvailable,
	})
	if err != nil {
		return fmt.Errorf("renderer: new map: %w", err)
	}
	w.m, err = mapFuture.Await(startupCtx)
	if err != nil {
		return fmt.Errorf("renderer: new map: %w", err)
	}

	// Core-worker driver with a dedicated (private) EGL context: the
	// native session worker creates and owns the context from our
	// display+config and needs no host graphics service. FrameWake shares
	// the wake channel with the runtime's EventWake — the callback only
	// schedules (non-blocking channel send) per the wake contract, and
	// the await drains both queues per turn regardless of which fired.
	attach, err := w.m.OpenglOwnedTextureAttach(
		maplibre.OpenglOwnedTextureDescriptor{
			Extent: maplibre.RenderTargetExtent{
				Width:       w.curW,
				Height:      w.curH,
				ScaleFactor: w.curScale,
			},
			Context: w.egl.descriptor(),
		},
		maplibre.RenderSessionAttachOptions{
			Driver:                    maplibre.RenderDriverKindCoreWorker,
			RequestedTextureRingDepth: 1,
			FrameWake:                 maplibre.Wake{Callback: wakeCallback},
		},
	)
	if err != nil {
		return fmt.Errorf("renderer: attach OpenGL owned texture render target: %w", err)
	}
	w.sess = attach.Session
	if _, err := attach.Completion.Await(startupCtx); err != nil {
		return fmt.Errorf("renderer: attach: %w", err)
	}

	caps, err := w.sess.GetCapabilities()
	if err != nil {
		return fmt.Errorf("renderer: session capabilities: %w", err)
	}
	if caps.Flags&maplibre.RenderSessionCapabilityFlagReadback == 0 {
		return fmt.Errorf("renderer: attached session does not grant readback; cannot render stills")
	}

	// Style loading proceeds on the native executor; the first render
	// observes any failure via MapLoadingFailed. Worker reports ready
	// as soon as the binding state is constructed.
	if _, err := w.m.SetStyleUrl(w.pool.cfg.styleURL); err != nil {
		return fmt.Errorf("renderer: set style url: %w", err)
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
		_ = w.sess.Close()
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
	if w.egl != nil {
		// Only after session detach: the display must stay initialized
		// through session teardown.
		w.egl.close()
	}
}

// renderOne is the per-worker hot path. Resize only if extent changed
// (saves the texture-realloc when consecutive requests share size).
func (w *goWorker) renderOne(ctx context.Context, vp ViewportRequest, scale int) (*image.NRGBA, error) {
	if vp.Width <= 0 || vp.Height <= 0 {
		return nil, fmt.Errorf("renderer: invalid dimensions %dx%d", vp.Width, vp.Height)
	}

	wantW := uint32(vp.Width)
	wantH := uint32(vp.Height)
	wantScale := float64(scale)
	var resize *maplibre.Future[maplibre.CommandCompletion]
	if wantW != w.curW || wantH != w.curH || wantScale != w.curScale {
		// Resize applies the extent and updates the map viewport. The
		// future only completes once a frame is produced at the new
		// extent (a static-mode map publishes no update without a
		// pending still), so rather than awaiting it here, let the still
		// image's demand loop below drive it to completion — session
		// operations are ordered ahead of the still's frames.
		f, err := w.sess.Resize(maplibre.RenderTargetExtent{
			Width:       wantW,
			Height:      wantH,
			ScaleFactor: wantScale,
		})
		if err != nil {
			return nil, fmt.Errorf("renderer: resize: %w", err)
		}
		resize = f
	}

	zoom, bearing, pitch := vp.Zoom, vp.Bearing, vp.Pitch
	if _, err := w.m.UpdateCamera(maplibre.CameraUpdate{
		Mode: maplibre.CameraUpdateModeJump,
		Camera: maplibre.CameraOptions{
			Center:  &maplibre.LatLng{Latitude: vp.Latitude, Longitude: vp.Longitude},
			Zoom:    &zoom,
			Bearing: &bearing,
			Pitch:   &pitch,
		},
	}); err != nil {
		return nil, fmt.Errorf("renderer: camera: %w", err)
	}

	still, err := w.m.RequestStillImage()
	if err != nil {
		return nil, fmt.Errorf("renderer: request still image: %w", err)
	}
	if err := w.awaitStill(ctx, still, w.pool.cfg.renderTimeout); err != nil {
		// The request may still be pending in the executor, and the map
		// refuses a second still while one is. Settle it (bounded, off
		// the caller's context) by driving the same await; poison the
		// worker if it cannot be settled.
		w.settleAbandonedStill(still)
		return nil, err
	}

	if resize != nil {
		// A rendered still at the new size implies the extent applied,
		// but completion can propagate a beat behind the frame result;
		// flush it rather than assuming.
		flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		cc, err := resize.Await(flushCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("renderer: resize: %w", err)
		}
		if cc.Disposition == maplibre.CommandDispositionFailed {
			return nil, fmt.Errorf("renderer: resize failed: %s", cc.Diagnostic)
		}
		w.curW, w.curH, w.curScale = wantW, wantH, wantScale
	}

	physW := vp.Width * scale
	physH := vp.Height * scale

	readStart := time.Now()
	rb, err := w.sess.TextureReadPremultipliedRgba8()
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, w.pool.cfg.renderTimeout)
	tr, err := rb.Await(readCtx)
	cancel()
	readDur := time.Since(readStart)
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	if int(tr.Info.Width) != physW || int(tr.Info.Height) != physH {
		return nil, fmt.Errorf("renderer: size mismatch: got %dx%d, want %dx%d", tr.Info.Width, tr.Info.Height, physW, physH)
	}
	// Guard the tight unpremultiply loop against a padded stride: it
	// assumes densely packed rows, and a mismatch would index past the
	// destination. Not expected from the RGBA8 readback; fail cleanly
	// rather than panic if an upstream change introduces padding.
	if len(tr.Data) != physW*physH*4 {
		return nil, fmt.Errorf("renderer: readback length mismatch: got %d bytes (stride %d), want %d", len(tr.Data), tr.Info.Stride, physW*physH*4)
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererReadback(w.pool.cfg.styleID, w.pool.cfg.scaleLabel, readDur.Seconds())
	}

	out := image.NewNRGBA(image.Rect(0, 0, physW, physH))
	unpremultiplyRGBA(out.Pix, tr.Data)
	return out, nil
}

// settleAbandonedStill drives an abandoned still request to completion,
// discarding whatever it produces. There is no cancel for a pending
// still and the map refuses a second one, so the only settle is the
// same demand-driven await, bounded and off the caller's (typically
// already cancelled) context. A worker whose still cannot settle within
// the budget is poisoned and retired by the pool.
func (w *goWorker) settleAbandonedStill(still *maplibre.Future[struct{}]) {
	if err := w.awaitStill(context.Background(), still, w.pool.cfg.settleTimeout); err != nil {
		w.poisoned = true
		slog.Warn("renderer worker retired: could not settle abandoned still-image request",
			"style", w.pool.cfg.styleID, "scale", w.pool.cfg.scaleLabel, "timeout", w.pool.cfg.settleTimeout, "error", err)
	}
}

// serviceTrace enables per-iteration stderr tracing of the still await.
// Debug aid for the executor conversion; cheap and env-gated.
var serviceTrace = os.Getenv("RENDERER_SERVICE_TRACE") != ""

// drainFrames drains and closes one frame-result batch, returning its
// results. ErrNotReady means an empty queue, reported as (nil, nil).
func (w *goWorker) drainFrames() ([]maplibre.RenderFrameResult, error) {
	fb, err := w.sess.DrainFrameResults()
	if err != nil {
		if errors.Is(err, maplibre.ErrNotReady) {
			return nil, nil
		}
		return nil, err
	}
	defer fb.Close()
	n, err := fb.Count()
	if err != nil {
		return nil, err
	}
	results := make([]maplibre.RenderFrameResult, 0, n)
	for i := uint(0); i < n; i++ {
		res, err := fb.Get(i)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// drainEvents drains and closes one runtime event batch. ErrNotReady
// means an empty queue, reported as (nil, nil).
func (w *goWorker) drainEvents() ([]maplibre.RuntimeEvent, error) {
	eb, err := w.rt.DrainEvents()
	if err != nil {
		if errors.Is(err, maplibre.ErrNotReady) {
			return nil, nil
		}
		return nil, err
	}
	defer eb.Close()
	view, err := eb.Get()
	if err != nil {
		return nil, err
	}
	// Copy out: the view's records belong to the batch handle.
	results := make([]maplibre.RuntimeEvent, len(view.Events))
	copy(results, view.Events)
	return results, nil
}

// awaitStill drives a still-image request to completion with
// update-driven frame demands, parking on the shared wake channel
// (runtime EventWake + session FrameWake). A static-mode map renders
// only on demand; rather than keep a speculative pipeline queued, the
// await issues exactly one demand per MapRenderUpdateAvailable event
// and one per rendered frame that reports NeedsRepaint — the map tells
// us when it has work, so there is never more than a single outstanding
// demand and the CoalescingBoundary supersede problem does not arise.
// Still completion arrives via its Future.
//
// This is the upstream canonical still loop (maplibre-native-ffi
// example zig-readback): demand on update, finish on the still, let the
// wake schedule the park. With one demand outstanding at a time there
// is no pre-pipeline leftover to draw the next render's first pass, so
// the upfront flush only discards results from a prior render's final
// in-flight demand.
func (w *goWorker) awaitStill(ctx context.Context, still *maplibre.Future[struct{}], budget time.Duration) error {
	deadline := time.Now().Add(budget)
	start := time.Now()
	iterations := 0
	demands := 0
	var totalWait time.Duration
	var timeToFirst time.Duration
	sawFirst := false

	// Flush results from an earlier render's last in-flight demand.
	// Everything drained after this point was produced with this
	// render's map state.
	for {
		stale, err := w.drainFrames()
		if err != nil {
			return fmt.Errorf("renderer: still image: flush frame results: %w", err)
		}
		if len(stale) == 0 {
			break
		}
	}
	// A wake latched by pre-flush results is stale too.
	select {
	case <-w.wake:
	default:
	}

	outstanding := 0
	// wantDemand latches a work signal (update event or repaint) until a
	// demand can carry it. Dropping a signal that arrives while a demand
	// is in flight stalls the still: if that in-flight demand resolves
	// NoUpdate, the map does not republish the update it already sent.
	wantDemand := false
	rendered := false
	completed := false

	issue := func() error {
		w.frameToken++
		demand := maplibre.FrameDemand{
			Flags:              maplibre.FrameDemandFlagIfNeeded,
			Token:              w.frameToken,
			CoalescingBoundary: w.frameToken,
		}
		if err := w.sess.RequestFrame(demand); err != nil {
			return fmt.Errorf("renderer: still image: request frame: %w", err)
		}
		if serviceTrace {
			fmt.Fprintf(os.Stderr, "TRACE +%6dus demand token=%d\n", time.Since(start).Microseconds(), demand.Token)
		}
		outstanding++
		demands++
		return nil
	}

	// Kick the first frame: the initial map state is the first "update".
	if err := issue(); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("renderer: still image timed out after %s", budget)
		}
		iterations++

		progressed := false
		results, err := w.drainFrames()
		if err != nil {
			return fmt.Errorf("renderer: still image: drain frame results: %w", err)
		}
		for _, res := range results {
			if serviceTrace {
				fmt.Fprintf(os.Stderr, "TRACE +%6dus it=%d frame token=%d disp=%d repaint=%t\n", time.Since(start).Microseconds(), iterations, res.Token, res.Disposition, res.NeedsRepaint)
			}
			progressed = true
			if !sawFirst {
				timeToFirst = time.Since(start)
				sawFirst = true
			}
			outstanding = 0
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.IncRendererStillFrameResult(w.pool.cfg.styleID, w.pool.cfg.scaleLabel, dispositionLabel(res.Disposition))
			}
			if res.Disposition == maplibre.RenderResultRendered {
				rendered = true
				// A rendered frame that still needs repaint has more to
				// draw: demand the follow-up pass.
				if res.NeedsRepaint {
					wantDemand = true
				}
			}
		}

		events, err := w.drainEvents()
		if err != nil {
			return fmt.Errorf("renderer: still image: drain events: %w", err)
		}
		for _, ev := range events {
			if serviceTrace {
				fmt.Fprintf(os.Stderr, "TRACE +%6dus it=%d event type=%d msg=%q\n", time.Since(start).Microseconds(), iterations, ev.Type, ev.Message)
			}
			switch ev.Type {
			case maplibre.RuntimeEventTypeMapLoadingFailed:
				return fmt.Errorf("renderer: map loading failed: %s", ev.Message)
			case maplibre.RuntimeEventTypeMapRenderError:
				return fmt.Errorf("renderer: map render error: %s", ev.Message)
			case maplibre.RuntimeEventTypeMapRenderUpdateAvailable:
				// The map has state or queued render-thread work; answer
				// with a frame demand.
				wantDemand = true
			}
		}

		if !completed {
			select {
			case <-still.Done():
				if serviceTrace {
					fmt.Fprintf(os.Stderr, "TRACE +%6dus it=%d still COMPLETED\n", time.Since(start).Microseconds(), iterations)
				}
				completed = true
				// Failure surfaces immediately; success still needs the
				// rendered frame result observed above.
				if _, err := still.Await(ctx); err != nil {
					return fmt.Errorf("renderer: still image: %w", err)
				}
			default:
			}
		}

		if completed && rendered {
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.RecordRendererPumpBreakdown(
					w.pool.cfg.styleID, w.pool.cfg.scaleLabel,
					iterations,
					totalWait.Seconds(),
					timeToFirst.Seconds(),
					0, // host performs no graphics work under the core-worker driver
					demands,
				)
			}
			return nil
		}

		// Issue a demand when the map signalled work this turn (update
		// event or a rendered-but-repaint frame), unless the still has
		// already completed — then we only await the final rendered
		// result, demanding nothing new.
		if !completed && wantDemand && outstanding == 0 {
			if err := issue(); err != nil {
				return err
			}
			wantDemand = false
		}

		if !progressed {
			// Nothing drained this turn: park until a wake fires (a
			// frame result or runtime event landed), the still
			// completes, or the safety cap passes — the cap is
			// missed-wake insurance, not pacing, so it is generous.
			waitStart := time.Now()
			timer := time.NewTimer(50 * time.Millisecond)
			if completed {
				// No new demands once completed; just wait for the
				// rendered result's wake.
				select {
				case <-w.wake:
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					totalWait += time.Since(waitStart)
					return ctx.Err()
				}
			} else {
				select {
				case <-w.wake:
				case <-still.Done():
				case <-timer.C:
					// A quiet park with nothing in flight means a work
					// signal was lost or never sent; an IF_NEEDED
					// keep-alive demand bounds that to one timer period
					// instead of the render timeout.
					if outstanding == 0 {
						wantDemand = true
						if services.GlobalMetrics != nil {
							services.GlobalMetrics.IncRendererStillKeepalive(w.pool.cfg.styleID, w.pool.cfg.scaleLabel)
						}
					}
				case <-ctx.Done():
					timer.Stop()
					totalWait += time.Since(waitStart)
					return ctx.Err()
				}
			}
			timer.Stop()
			totalWait += time.Since(waitStart)
		}
	}
}

func dispositionLabel(d maplibre.RenderResult) string {
	switch d {
	case maplibre.RenderResultRendered:
		return "rendered"
	case maplibre.RenderResultNoUpdate:
		return "no_update"
	case maplibre.RenderResultSizePending:
		return "size_pending"
	case maplibre.RenderResultTargetNotReady:
		return "target_not_ready"
	case maplibre.RenderResultSuperseded:
		return "superseded"
	case maplibre.RenderResultDeadlineMissed:
		return "deadline_missed"
	default:
		return "other"
	}
}

// unpremultiplyRGBA converts premultiplied RGBA bytes (binding output)
// to non-premultiplied RGBA (image.NRGBA layout). Pure byte loop — hot
// enough on the render path that we keep the implementation inline.
func unpremultiplyRGBA(dst, src []byte) {
	for i := 0; i < len(src); i += 4 {
		r, g, b, a := src[i], src[i+1], src[i+2], src[i+3]
		switch a {
		case 0, 255:
			dst[i+0], dst[i+1], dst[i+2], dst[i+3] = r, g, b, a
		default:
			ai := uint32(a)
			dst[i+0] = byte((uint32(r)*255 + ai/2) / ai)
			dst[i+1] = byte((uint32(g)*255 + ai/2) / ai)
			dst[i+2] = byte((uint32(b)*255 + ai/2) / ai)
			dst[i+3] = a
		}
	}
}

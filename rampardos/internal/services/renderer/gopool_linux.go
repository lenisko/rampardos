// Package renderer's in-process implementation, driving maplibre-native
// through its C ABI. Linux-only: it needs EGL and links
// libmaplibre-native-c.so. The _linux suffix is the whole constraint —
// non-Linux builds get the stub in gopool_other.go.
//
// Per-pool internals are runtime.LockOSThread-pinned worker goroutines
// because the binding requires every native call for a Runtime to
// originate on the thread that created it.
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
// surfaceless. One worker goroutine per pool slot, each pinned to an
// OS thread (the upstream binding's RuntimeHandle is thread-affine to
// the OS thread that created it). Same outer shape as the previous
// jfberry-based implementation: lazy (style, scale)-keyed pools,
// two-level concurrency (process-global semaphore × per-pool slot
// count), encode at the renderer boundary.
type GoPoolRenderer struct {
	cfg Config
	sem *semaphore.Weighted

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
		cfg.StylePoolIdleTTL = 2 * time.Minute
	}
	if cfg.RenderTimeout <= 0 {
		cfg.RenderTimeout = 15 * time.Second
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 30 * time.Second
	}

	// Catch a wrong-variant FFI at process start, not at first render.
	backends := maplibre.SupportedRenderBackends()
	if !backends.Has(maplibre.RenderBackendOpenGL) {
		return nil, fmt.Errorf("renderer: linked FFI does not advertise OpenGL backend; check Dockerfile MISE_ENV variant (expected linux-<arch>-egl)")
	}

	if _, err := cfg.DiscoverStyles(); err != nil {
		return nil, fmt.Errorf("renderer: discover styles: %w", err)
	}

	r := &GoPoolRenderer{
		cfg:   cfg,
		sem:   semaphore.NewWeighted(int64(cfg.PoolSize)),
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
	scale := int(req.Scale)
	if scale < 1 {
		scale = 1
	}

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
	return nil
}

// goStylePoolConfig captures the per-pool parameters. Pool members are
// scale-segregated; ratio is therefore baked in at pool construction.
type goStylePoolConfig struct {
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
// RenderSessionHandle + eglContext for its lifetime.
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
		cfg.idleTTL = 2 * time.Minute
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

// goWorker is one pool slot. Owns one OS thread, one EGL context, one
// Runtime, one Map, one RenderSession. Never accessed from outside its
// own loop() goroutine.
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
	egl  *eglContext
	buf  []byte // reusable readback buffer

	curW     uint32
	curH     uint32
	curScale float64

	// stillPending mirrors the FFI's own pending-still flag: set when
	// RequestStillImage succeeds, cleared when a terminal still event is
	// observed. Only an outstanding request needs settling — a render that
	// failed *because* of StillImageFailed has already been cleared inside
	// the FFI, and settling that would block for the whole budget and
	// retire a perfectly healthy worker.
	stillPending bool

	// poisoned is set when an abandoned still-image request could not be
	// settled. The map cannot start another render, so the worker exits
	// after replying and the pool replaces it on the next demand.
	poisoned bool
}

func (w *goWorker) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
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
			// Retired by the reaper. Deferred cleanup releases the EGL
			// context, map, runtime and their mbgl caches — the whole
			// point of shrinking.
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
			// SetStyleURL is async — the new style starts loading in
			// the background and the next render's pump drives it
			// forward (same pattern as init). Reply immediately on
			// the SetStyleURL return so reload doesn't block reads.
			err := w.m.SetStyleURL(cmd.reload)
			cmd.reply <- goWorkerResult{err: err}
		}
	}
}

func (w *goWorker) init() error {
	var err error
	w.egl, err = newEGLContext()
	if err != nil {
		return err
	}

	w.rt, err = maplibre.NewRuntime()
	if err != nil {
		return fmt.Errorf("renderer: new runtime: %w", err)
	}

	// MapModeStatic is the render-once mode used by the upstream
	// example. Continuous mode is for live-pan; static mode disables
	// the animation tick and aligns with our request/reply pattern.
	w.curW, w.curH, w.curScale = 256, 256, float64(w.pool.cfg.ratio)
	w.m, err = w.rt.NewMapWithOptions(maplibre.MapOptions{
		Width:       w.curW,
		Height:      w.curH,
		ScaleFactor: w.curScale,
		Mode:        maplibre.MapModeStatic,
	})
	if err != nil {
		return fmt.Errorf("renderer: new map: %w", err)
	}

	// Session-owned texture. We never call AcquireOpenGLTextureFrame —
	// output leaves via CPU readback (ReadPremultipliedRGBA8Into) — and
	// that is exactly the path upstream optimised in FFI #398: the
	// texture session's swap() only issues glFinish() for *caller-owned*
	// (borrowed) textures, which are handed back every frame. A
	// session-owned texture defers its GPU completion to acquire-frame,
	// and CPU readback fences on glReadPixels instead. So this attach
	// gets the per-frame glFinish removed without the cross-context
	// hazard that made removing it outright unsafe (see FFI #281).
	w.sess, err = w.m.AttachOpenGLOwnedTexture(maplibre.OpenGLOwnedTextureDescriptor{
		Extent: maplibre.RenderTargetExtent{
			Width:       w.curW,
			Height:      w.curH,
			ScaleFactor: w.curScale,
		},
		Context: w.egl.descriptor(),
	})
	if err != nil {
		return fmt.Errorf("renderer: attach OpenGL owned texture render target: %w", err)
	}

	// Set style URL but don't wait for the style-loaded event in init.
	// Style loading happens asynchronously inside the runtime; the
	// first render's pump will drive it forward. Worker reports ready
	// as soon as the binding state is constructed.
	if err := w.m.SetStyleURL(w.pool.cfg.styleURL); err != nil {
		return fmt.Errorf("renderer: set style url: %w", err)
	}

	return nil
}

func (w *goWorker) cleanup() {
	if w.sess != nil {
		_ = w.sess.Close()
	}
	if w.m != nil {
		_ = w.m.Close()
	}
	if w.rt != nil {
		_ = w.rt.Close()
	}
	if w.egl != nil {
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
	if wantW != w.curW || wantH != w.curH || wantScale != w.curScale {
		if err := w.sess.Resize(maplibre.RenderTargetExtent{
			Width:       wantW,
			Height:      wantH,
			ScaleFactor: wantScale,
		}); err != nil {
			return nil, fmt.Errorf("renderer: resize: %w", err)
		}
		w.curW, w.curH, w.curScale = wantW, wantH, wantScale
	}

	if err := w.m.JumpTo(maplibre.CameraOptions{}.
		WithCenter(maplibre.LatLng{Latitude: vp.Latitude, Longitude: vp.Longitude}).
		WithZoom(vp.Zoom).
		WithBearing(vp.Bearing).
		WithPitch(vp.Pitch)); err != nil {
		return nil, fmt.Errorf("renderer: camera: %w", err)
	}

	if err := w.m.RequestStillImage(); err != nil {
		return nil, fmt.Errorf("renderer: request still image: %w", err)
	}
	w.stillPending = true
	if err := w.pumpUntilStillFinished(ctx, w.pool.cfg.renderTimeout); err != nil {
		// The still request is still outstanding in mbgl: the FFI clears
		// its pending flag only from the completion callback, which needs
		// the runloop driven. Abandoning here without settling leaves the
		// map permanently unable to start another render — every later
		// RequestStillImage on this worker returns INVALID_STATE, so one
		// cancelled request would poison the worker for the process
		// lifetime. Drive it to completion (bounded, and deliberately not
		// on the caller's context, which is typically already cancelled)
		// and discard the frame.
		if w.stillPending {
			w.settleAbandonedStill()
		}
		return nil, err
	}

	physW := vp.Width * scale
	physH := vp.Height * scale
	want := physW * physH * 4

	if cap(w.buf) < want {
		w.buf = make([]byte, want)
	} else {
		w.buf = w.buf[:want]
	}
	readStart := time.Now()
	info, err := w.sess.ReadPremultipliedRGBA8Into(w.buf)
	readDur := time.Since(readStart)
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	if int(info.Width) != physW || int(info.Height) != physH {
		return nil, fmt.Errorf("renderer: size mismatch: got %dx%d, want %dx%d", info.Width, info.Height, physW, physH)
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererReadback(w.pool.cfg.styleID, w.pool.cfg.scaleLabel, readDur.Seconds())
	}

	out := image.NewNRGBA(image.Rect(0, 0, physW, physH))
	unpremultiplyRGBA(out.Pix, w.buf)
	return out, nil
}

// settleAbandonedStill drives the runtime until an outstanding still
// request completes, discarding whatever it produces. Marks the worker
// unusable if it cannot be settled, so the pool retires it rather than
// handing out a map that can never start another render.
//
// Bounded independently of the render deadline: the render is usually
// nearly done (the common trigger is a client disconnect part-way), so
// this normally returns in single-digit milliseconds.
func (w *goWorker) settleAbandonedStill() {
	deadline := time.Now().Add(w.pool.cfg.settleTimeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if err := w.rt.Pump(remaining); err != nil {
			break
		}
		for {
			ev, err := w.rt.PollEvent()
			if err != nil || ev == nil {
				break
			}
			switch ev.Type {
			case maplibre.RuntimeEventMapRenderUpdateAvailable:
				// mbgl only completes the still from inside a render, so
				// the draws have to keep happening for the callback that
				// clears the pending flag to ever run.
				if _, err := w.sess.RenderUpdate(); err != nil {
					w.poisoned = true
					return
				}
			case maplibre.RuntimeEventMapStillImageFinished,
				maplibre.RuntimeEventMapStillImageFailed:
				w.stillPending = false
				return
			}
		}
	}

	// Could not settle: the map still holds a pending request, so this
	// worker can never render again. Retiring it lets the pool replace it.
	w.poisoned = true
	slog.Warn("renderer worker retired: could not settle abandoned still-image request",
		"style", w.pool.cfg.styleID, "scale", w.pool.cfg.scaleLabel, "timeout", w.pool.cfg.settleTimeout)
}

// pumpUntilStillFinished drives the runtime event loop until the
// current still-image render completes (or the budget expires).
//
// Pump parks the OS thread until the runtime has work — a wake flag
// latched by style/tile/offline/resource responses and by queued
// runtime events — then drains the owner-thread task queues. It
// returns without parking while unread events are already queued, so
// the loop never sleeps on work it could be doing.
//
// This replaced a sleep-backoff pump, which was the real cost in the
// pre-upstream design: mbgl's frontend update() queues to the
// runtime's event queue, not libuv's, so a non-blocking run_once
// returned having advanced nothing and the caller had to sleep. At
// ~15 iterations per cold render a ~0.5-1 ms sleep each, that was
// most of the measured per-render gap — three orders of magnitude
// more than the cgo crossings themselves (~50-100 ns each).
//
// pumpSleep below measures cumulative time inside Pump, which is now
// genuine mbgl I/O wait (worker threads parsing tiles, file-source
// queries) rather than scheduler backoff.
func (w *goWorker) pumpUntilStillFinished(ctx context.Context, budget time.Duration) error {
	rendered := false
	deadline := time.Now().Add(budget)

	// Per-render breakdown stats. Emitted to Prometheus on the success
	// path so we can see whether render time is dominated by pump
	// iterations (cgo overhead), sleep backoff (idle waiting), mbgl
	// warm-up (time to first event = file-source / tile-fetch wait),
	// or the actual render work (RenderUpdate cgo cost). renderUpdateCnt
	// captures multi-pass behaviour — mbgl may emit multiple
	// RenderUpdateAvailable events per still image as tiles arrive
	// progressively; each one triggers a full draw.
	pumpStart := time.Now()
	iterations := 0
	renderUpdateCnt := 0
	var totalSleep, totalRenderUpdate time.Duration
	var timeToFirstEvent time.Duration
	firstEventSeen := false

	// drain polls every queued event, folding render-updates into a single
	// pending flag. Returns whether it saw any event, and whether the still
	// image completed. Extracted so the batching sweep below can re-drain
	// without duplicating the event switch.
	var pendingRenderUpdate bool
	drain := func() (sawEvent bool, finished bool, err error) {
		for {
			ev, err := w.rt.PollEvent()
			if err != nil {
				return sawEvent, false, fmt.Errorf("renderer: PollEvent: %w", err)
			}
			if ev == nil {
				return sawEvent, false, nil
			}
			sawEvent = true
			if !firstEventSeen {
				timeToFirstEvent = time.Since(pumpStart)
				firstEventSeen = true
			}
			switch ev.Type {
			case maplibre.RuntimeEventMapRenderUpdateAvailable:
				pendingRenderUpdate = true
			case maplibre.RuntimeEventMapStillImageFinished:
				w.stillPending = false
				return sawEvent, true, nil
			case maplibre.RuntimeEventMapLoadingFailed:
				return sawEvent, false, fmt.Errorf("renderer: map loading failed: %s", ev.Message)
			case maplibre.RuntimeEventMapRenderError:
				return sawEvent, false, fmt.Errorf("renderer: map render error: %s", ev.Message)
			case maplibre.RuntimeEventMapStillImageFailed:
				w.stillPending = false
				return sawEvent, false, fmt.Errorf("renderer: still image failed: %s", ev.Message)
			}
		}
	}

	// flush draws the latest update, if one is pending.
	flush := func() error {
		if !pendingRenderUpdate {
			return nil
		}
		ruStart := time.Now()
		drew, err := w.sess.RenderUpdate()
		if err != nil {
			return fmt.Errorf("renderer: RenderUpdate: %w", err)
		}
		totalRenderUpdate += time.Since(ruStart)
		renderUpdateCnt++
		rendered = rendered || drew
		pendingRenderUpdate = false
		return nil
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("renderer: still image timed out after %s", budget)
		}
		iterations++

		// Park until the runtime has work, bounded by the remaining
		// budget so the outer deadline check stays authoritative.
		blockStart := time.Now()
		if err := w.rt.Pump(remaining); err != nil {
			totalSleep += time.Since(blockStart)
			return fmt.Errorf("renderer: pump: %w", err)
		}
		totalSleep += time.Since(blockStart)

		_, finished, err := drain()
		if err != nil {
			return err
		}

		if err := flush(); err != nil {
			return err
		}

		if finished {
			if !rendered {
				return fmt.Errorf("renderer: still image finished without a render frame")
			}
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.RecordRendererPumpBreakdown(
					w.pool.cfg.styleID, w.pool.cfg.scaleLabel,
					iterations,
					totalSleep.Seconds(),
					timeToFirstEvent.Seconds(),
					totalRenderUpdate.Seconds(),
					renderUpdateCnt,
				)
			}
			return nil
		}
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

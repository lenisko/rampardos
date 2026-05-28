//go:build mln_ffi

// Package renderer's Go-binding-backed implementation. Compiled only
// when -tags mln_ffi is set, because the import of
// github.com/maplibre/maplibre-native-ffi/bindings/go pulls in CGO and
// the libmaplibre-native-c.so shared library. Standard rampardos builds
// (no tag) get the stub from gopool_stub.go.
//
// Spike scope (see docs/superpowers/specs/2026-05-27-go-renderer-opengl-spike-design.md):
// upstream binding, EGL OpenGL backend only, single-variant link. The
// outer renderer.Renderer interface is unchanged; the per-pool internals
// are rewritten as runtime.LockOSThread-pinned worker goroutines because
// the upstream binding requires every native call to originate on the
// thread that created the Runtime.
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
	poolSize        int
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

	mu     sync.Mutex
	closed bool
}

type goWorkerCmdKind int

const (
	goWorkerCmdRender goWorkerCmdKind = iota
	goWorkerCmdReload
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

	p := &goStylePool{
		cfg:  cfg,
		cmds: make(chan goWorkerCommand),
	}

	startupErrs := make(chan error, cfg.poolSize)
	for i := 0; i < cfg.poolSize; i++ {
		p.wg.Add(1)
		go (&goWorker{
			pool:        p,
			startupErrs: startupErrs,
		}).loop()
	}

	// Wait for all workers to report startup status. First failure
	// triggers a teardown of any already-started workers.
	deadline := time.NewTimer(cfg.startupTimeout)
	defer deadline.Stop()
	var firstErr error
	collected := 0
	for collected < cfg.poolSize {
		select {
		case err := <-startupErrs:
			collected++
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-deadline.C:
			firstErr = fmt.Errorf("renderer: pool startup timeout after %s (workers ready %d/%d)", cfg.startupTimeout, collected, cfg.poolSize)
			collected = cfg.poolSize // break the loop
		}
	}
	if firstErr != nil {
		// Close the cmd channel so any already-running workers exit
		// their loops and tear down their state. Wait for them.
		close(p.cmds)
		p.wg.Wait()
		return nil, fmt.Errorf("renderer: build pool for style %q ratio=%d: %w", cfg.styleID, cfg.ratio, firstErr)
	}

	return p, nil
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

	select {
	case p.cmds <- cmd:
		// fall through to wait for reply
	case <-ctx.Done():
		return nil, ctx.Err()
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
	replies := make([]chan goWorkerResult, p.cfg.poolSize)
	for i := 0; i < p.cfg.poolSize; i++ {
		replies[i] = make(chan goWorkerResult, 1)
		cmd := goWorkerCommand{
			kind:   goWorkerCmdReload,
			ctx:    ctx,
			reload: styleURL,
			reply:  replies[i],
		}
		select {
		case p.cmds <- cmd:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var firstErr error
	for i := 0; i < p.cfg.poolSize; i++ {
		select {
		case res := <-replies[i]:
			if res.err != nil && firstErr == nil {
				firstErr = res.err
			}
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
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
	close(p.cmds)
	p.wg.Wait()
}

// goWorker is one pool slot. Owns one OS thread, one EGL context, one
// Runtime, one Map, one RenderSession. Never accessed from outside its
// own loop() goroutine.
type goWorker struct {
	pool        *goStylePool
	startupErrs chan<- error

	rt   *maplibre.RuntimeHandle
	m    *maplibre.MapHandle
	sess *maplibre.RenderSessionHandle
	egl  *eglContext
	buf  []byte // reusable readback buffer

	curW     uint32
	curH     uint32
	curScale float64
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

	for cmd := range w.pool.cmds {
		switch cmd.kind {
		case goWorkerCmdRender:
			img, err := w.renderOne(cmd.ctx, cmd.render.vp, cmd.render.scale)
			cmd.reply <- goWorkerResult{img: img, err: err}
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

	// AttachOpenGLOffscreen: framebuffer with a renderbuffer color
	// attachment (no exposed texture handle). Matches mbgl's
	// HeadlessBackend layout, which the Node binding uses — avoids the
	// texture-attached FBO's per-render sync overhead on Mesa software
	// stacks. We only need CPU readback via
	// RenderSessionHandle.ReadPremultipliedRGBA8Into, so the texture
	// handle the owned-texture path exposed was never used anyway.
	w.sess, err = w.m.AttachOpenGLOffscreen(maplibre.OpenGLOffscreenDescriptor{
		Extent: maplibre.RenderTargetExtent{
			Width:       w.curW,
			Height:      w.curH,
			ScaleFactor: w.curScale,
		},
		Context: w.egl.descriptor(),
	})
	if err != nil {
		return fmt.Errorf("renderer: attach OpenGL offscreen render target: %w", err)
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

	if err := w.pumpUntilStillFinished(ctx, w.pool.cfg.renderTimeout); err != nil {
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

// pumpUntilStillFinished drives the runtime event loop until the
// current still-image render completes (or the budget expires).
//
// Idle backoff: time.Sleep(100µs). On Linux this rounds up to
// kernel timer resolution (~1ms on CONFIG_HZ=1000). The instrumented
// breakdown shows ~14ms of accumulated sleep wait per render at
// scale=1, but this is NOT wasted time — it's mbgl waiting for tile
// workers to deliver parsed tile/glyph/sprite data via worker
// threads. The actual unrecoverable cost is wake-up latency: when
// mbgl emits an event mid-sleep, we don't notice until the kernel
// timer fires, losing ~500µs per event on average. Node's libuv
// pump uses uv_run(UV_RUN_ONCE) which blocks on epoll_wait and
// wakes in microseconds when events become ready, saving ~6ms per
// render vs our 1ms-granularity sleep.
//
// runtime.Gosched() was tried as an alternative and turned out
// worse: ~18µs per call due to scheduler contention with HTTP
// handlers + dispatcher goroutines, plus tighter polling causes
// mbgl to emit MORE incremental render updates (~16 vs ~12),
// increasing total RenderUpdate cgo time. Net: +3ms worse.
//
// The real fix would be an FFI-side mln_runtime_run_blocking that
// exposes uv_run(UV_RUN_ONCE). With microsecond-latency wake-up
// our p50 should drop from ~28ms to ~16ms (matching/beating Node's
// 20ms). Tracked as a follow-up FFI request.
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

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		iterations++
		if err := w.rt.RunOnce(); err != nil {
			return fmt.Errorf("renderer: RunOnce: %w", err)
		}
		productive := false
		for {
			ev, err := w.rt.PollEvent()
			if err != nil {
				return fmt.Errorf("renderer: PollEvent: %w", err)
			}
			if ev == nil {
				break
			}
			if !firstEventSeen {
				timeToFirstEvent = time.Since(pumpStart)
				firstEventSeen = true
			}
			productive = true
			switch ev.Type {
			case maplibre.RuntimeEventMapRenderUpdateAvailable:
				ruStart := time.Now()
				if err := w.sess.RenderUpdate(); err != nil {
					return fmt.Errorf("renderer: RenderUpdate: %w", err)
				}
				totalRenderUpdate += time.Since(ruStart)
				renderUpdateCnt++
				rendered = true
			case maplibre.RuntimeEventMapStillImageFinished:
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
			case maplibre.RuntimeEventMapLoadingFailed:
				return fmt.Errorf("renderer: map loading failed: %s", ev.Message)
			case maplibre.RuntimeEventMapRenderError:
				return fmt.Errorf("renderer: map render error: %s", ev.Message)
			case maplibre.RuntimeEventMapStillImageFailed:
				return fmt.Errorf("renderer: still image failed: %s", ev.Message)
			}
		}
		// Idle iterations sleep 100µs (rounds up to ~1ms on Linux
		// CONFIG_HZ=1000). See block comment above — sleep is mbgl
		// waiting for worker threads, not wasted time. The wake-up
		// latency loss vs Node's epoll-based pump is the remaining
		// structural gap.
		if !productive {
			sleepStart := time.Now()
			time.Sleep(100 * time.Microsecond)
			totalSleep += time.Since(sleepStart)
		}
	}
	return fmt.Errorf("renderer: still image timed out after %s", budget)
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

//go:build mln_ffi

// Package renderer's Go-binding-backed implementation. Compiled only
// when -tags mln_ffi is set, because the import of
// github.com/jfberry/maplibre-native-go pulls in CGO and the libmln_ffi
// shared library. Standard rampardos builds (no tag) get the stub from
// gopool_stub.go, which surfaces a clear error if RENDERER_BACKEND=go-pool
// is selected without the library available.
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

	maplibre "github.com/jfberry/maplibre-native-go"
	"github.com/lenisko/rampardos/internal/fileutil"
	"github.com/lenisko/rampardos/internal/services"
	"golang.org/x/sync/semaphore"
)

// Ensure GoPoolRenderer satisfies the Renderer interface.
var _ Renderer = (*GoPoolRenderer)(nil)

// GoPoolRenderer is the in-process Renderer implementation backed by
// the Go binding to maplibre-native (one *maplibre.Session per worker
// slot, one OS thread per Session). Mirrors NodePoolRenderer's outer
// shape: lazy (style, scale)-keyed pools, two-level concurrency
// (process-global semaphore × per-pool slot count), encode at the
// renderer boundary so callers see encoded image bytes.
//
// Trade-offs vs the Node backend, summarised:
//   - One process, no IPC framing on the hot path.
//   - In-place style swap via Session.SetStyleURL — ReloadStyles
//     mutates Sessions instead of rebuilding pools.
//   - Crash isolation is gone — a C++ segfault in mbgl takes
//     rampardos down. Mitigated by supervisor restart and a
//     per-render Go-side recover (handled at call sites; this
//     renderer just returns errors).
//
// Caching, singleflight, expiry-queue, and the static-map dispatcher
// sit above the Renderer interface and are unchanged.
type GoPoolRenderer struct {
	cfg Config
	sem *semaphore.Weighted

	mu    sync.RWMutex
	pools map[string]*goStylePool
}

// NewGoPoolRenderer returns an in-process renderer using a Session pool.
// Pools are created lazily on first use of each (styleID, scale) tuple.
// Returns Renderer (not *GoPoolRenderer) so the build-tag stub in
// gopool_stub.go can match the signature exactly without adding a
// build-tagged factory shim in main.go.
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
		// Style load including font/sprite/source resolution can take a
		// few seconds on first run; keep the bar generous.
		cfg.StartupTimeout = 30 * time.Second
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

// acquireGlobal / releaseGlobal mirror NodePoolRenderer so the existing
// rampardos_renderer_global_* metrics keep meaning under either backend.
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

// Render satisfies Renderer. Tile path: convert to viewport, dispatch
// through the per-pool semaphore, encode.
func (r *GoPoolRenderer) Render(ctx context.Context, req Request) ([]byte, error) {
	vp := TileToViewport(req.Z, req.X, req.Y, req.Scale)
	vp.StyleID = req.StyleID
	vp.Format = req.Format
	img, err := r.renderViewportImage(ctx, vp, false /* applyZoomAdj */)
	if err != nil {
		return nil, err
	}
	return encodeRGBAImage(img, req.Format)
}

// RenderViewport returns encoded bytes for an arbitrary viewport.
// Applies the styleZoomOffset adjustment, matching NodePoolRenderer's
// RenderViewport semantics (web-map convention zoom in, MapLibre
// convention zoom internally).
func (r *GoPoolRenderer) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
	img, err := r.RenderViewportImage(ctx, req)
	if err != nil {
		return nil, err
	}
	return encodeRGBAImage(img, req.Format)
}

// RenderViewportImage returns an *image.NRGBA so callers compositing in
// memory can skip the encode/decode round-trip.
func (r *GoPoolRenderer) RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error) {
	return r.renderViewportImage(ctx, req, true /* applyZoomAdj */)
}

// renderViewportImage is the shared dispatcher behind Render and
// RenderViewportImage. The applyZoomAdj flag captures the
// nodepool.go convention: tile renders pass coordinates that already
// match MapLibre's internal zoom unit (TileToViewport produces a
// 512px frame); arbitrary viewport renders pass web-map convention
// zoom and must be adjusted by the per-style offset.
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

// loadPool resolves the on-disk style.json, runs PrepareStyle to rewrite
// source URLs into the canonical mbtiles://… form (which the C++ engine's
// DefaultFileSource resolves natively, so unlike the Node worker we do
// not need a per-process custom request callback), atomic-writes the
// prepared file, and fans out N Sessions in parallel. If any session
// fails to construct, all already-built ones are closed.
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
	}
	return newGoStylePool(cfg)
}

// getOrCreatePool returns the (style, scale) pool, creating it lazily.
// Same DCL pattern as NodePoolRenderer.getOrCreatePool — fast RLock for
// the steady state, slow write lock only on first reference.
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

// ReloadStyles swaps each pool's Sessions to the new style in place,
// in parallel within a pool, sequentially across pools. The dramatic
// simplification vs the Node path is that no Session is destroyed: each
// SetStyleURL reuses the underlying *Map and rebuilds only the style
// graph, not the renderer/Session/Runtime stack.
//
// The merge-not-overwrite race that NodePoolRenderer.ReloadStyles
// handles for concurrent getOrCreatePool callers does not apply here:
// no entry in r.pools is replaced, only mutated.
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
			// Closed underneath us — skip rather than fail the whole reload.
			continue
		}

		styleID, ratio := parsePoolKey(key)

		// Re-prepare the style from disk before swap. Style files may
		// have been edited; without re-reading we'd swap to a stale
		// prepared.json.
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
			slog.Error("Reload: SetStyleAll failed; pool may be partially swapped", "style", styleID, "ratio", ratio, "error", err)
			continue
		}
		// Update zoomAdj after a successful swap. tileSize changes
		// across reloads are rare but the offset is style-dependent.
		pool.mu.Lock()
		pool.cfg.viewportZoomAdj = zoomAdj
		pool.cfg.styleURL = styleURL
		pool.mu.Unlock()
	}
	return nil
}

// Close drains and disposes every Session in every pool. Safe to call
// multiple times (close is idempotent at the pool level).
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
}

// goStylePool owns N Sessions for one (style, scale) tuple.
type goStylePool struct {
	cfg goStylePoolConfig

	sessions chan *maplibre.Session
	bufPool  sync.Pool // []byte for RenderInto reuse, sized to physical pixels

	mu     sync.Mutex
	closed bool
}

// newGoStylePool fans out poolSize NewSession calls in parallel — each
// blocks waiting for STYLE_LOADED, so serial construction would scale
// linearly with pool size. If any session fails, every already-built
// one is closed and the first error is returned.
func newGoStylePool(cfg goStylePoolConfig) (*goStylePool, error) {
	if cfg.poolSize <= 0 {
		return nil, fmt.Errorf("renderer: pool size must be > 0")
	}
	if cfg.startupTimeout <= 0 {
		cfg.startupTimeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.startupTimeout)
	defer cancel()

	type result struct {
		idx int
		s   *maplibre.Session
		err error
	}
	results := make(chan result, cfg.poolSize)
	for i := 0; i < cfg.poolSize; i++ {
		go func(i int) {
			s, err := maplibre.NewSession(ctx, maplibre.SessionOptions{
				// Width/Height are placeholders — every render path
				// calls Resize before JumpTo, so the initial values do
				// not matter beyond "valid non-zero".
				Map:   maplibre.MapOptions{Width: 256, Height: 256, ScaleFactor: float64(cfg.ratio)},
				Style: cfg.styleURL,
			})
			results <- result{idx: i, s: s, err: err}
		}(i)
	}

	sessions := make([]*maplibre.Session, cfg.poolSize)
	var firstErr error
	for i := 0; i < cfg.poolSize; i++ {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		sessions[r.idx] = r.s
	}
	if firstErr != nil {
		for _, s := range sessions {
			if s != nil {
				_ = s.Close()
			}
		}
		return nil, fmt.Errorf("renderer: build session pool for style %q ratio=%d: %w", cfg.styleID, cfg.ratio, firstErr)
	}

	p := &goStylePool{
		cfg:      cfg,
		sessions: make(chan *maplibre.Session, cfg.poolSize),
	}
	for _, s := range sessions {
		p.sessions <- s
	}
	return p, nil
}

// dispatch acquires a Session, runs one render, and returns the
// session to the channel. Mirrors stylePool.dispatch but the inner
// "worker died" handling is gone — Sessions don't die mid-render in
// the binding's contract; binding errors are reported synchronously
// via the return value and the Session remains usable.
func (p *goStylePool) dispatch(ctx context.Context, vp ViewportRequest, scale int) (*image.NRGBA, error) {
	acquireStart := time.Now()
	select {
	case s := <-p.sessions:
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordRendererPoolAcquire(p.cfg.styleID, p.cfg.scaleLabel, time.Since(acquireStart).Seconds(), len(p.sessions))
		}
		img, err := p.renderOne(ctx, s, vp, scale)
		p.releaseSession(s)
		return img, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// renderOne is the per-Session hot path: Resize → JumpTo → RenderInto →
// unpremultiply. Resize is unconditional because it's cheap (~µs) when
// dimensions are unchanged; conditional Resize would require tracking
// per-Session size and serialising that read against concurrent
// callers, which is more complex than the savings warrant.
func (p *goStylePool) renderOne(ctx context.Context, s *maplibre.Session, vp ViewportRequest, scale int) (*image.NRGBA, error) {
	if vp.Width <= 0 || vp.Height <= 0 {
		return nil, fmt.Errorf("renderer: invalid dimensions %dx%d", vp.Width, vp.Height)
	}
	if err := s.Resize(uint32(vp.Width), uint32(vp.Height), float64(scale)); err != nil {
		return nil, fmt.Errorf("renderer: resize: %w", err)
	}
	if err := s.JumpTo(maplibre.Camera{
		Fields: maplibre.CameraFieldCenter | maplibre.CameraFieldZoom |
			maplibre.CameraFieldBearing | maplibre.CameraFieldPitch,
		Latitude:  vp.Latitude,
		Longitude: vp.Longitude,
		Zoom:      vp.Zoom,
		Bearing:   vp.Bearing,
		Pitch:     vp.Pitch,
	}); err != nil {
		return nil, fmt.Errorf("renderer: camera: %w", err)
	}

	physW := vp.Width * scale
	physH := vp.Height * scale
	want := physW * physH * 4

	buf := p.acquireBuf(want)
	w, h, err := s.RenderInto(ctx, buf)
	if err != nil {
		p.releaseBuf(buf)
		return nil, fmt.Errorf("renderer: render: %w", err)
	}
	if w != physW || h != physH {
		p.releaseBuf(buf)
		return nil, fmt.Errorf("renderer: size mismatch: got %dx%d, want %dx%d", w, h, physW, physH)
	}

	// Binding emits premultiplied RGBA; image.NRGBA expects
	// non-premultiplied. Unpremultiply into a fresh buffer (the NRGBA
	// will outlive the render, so we don't pool it).
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	unpremultiplyRGBA(out.Pix, buf)
	p.releaseBuf(buf)
	return out, nil
}

// setStyleAll swaps every Session in the pool to the new style in
// parallel and blocks until all return. Each Session is taken out of
// the channel for the duration of its own swap so concurrent renders
// see either the old style fully or the new style fully — never a
// half-swapped state.
func (p *goStylePool) setStyleAll(ctx context.Context, styleURL string) error {
	// Drain — gather all sessions in a slice. Renders that arrive
	// during this window block on the now-empty channel until we
	// re-push.
	sessions := make([]*maplibre.Session, 0, p.cfg.poolSize)
	for i := 0; i < p.cfg.poolSize; i++ {
		select {
		case s := <-p.sessions:
			sessions = append(sessions, s)
		case <-ctx.Done():
			// Push back what we have and bail.
			for _, s := range sessions {
				p.sessions <- s
			}
			return ctx.Err()
		}
	}

	errs := make(chan error, len(sessions))
	for _, s := range sessions {
		go func(s *maplibre.Session) {
			errs <- s.SetStyleURL(ctx, styleURL)
		}(s)
	}
	var firstErr error
	for range sessions {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Whether or not the swap succeeded for every session, return them
	// all to the channel. Sessions whose swap failed are still usable
	// (likely on the previous style); a partial-failure mode is logged
	// by the caller in ReloadStyles.
	for _, s := range sessions {
		p.sessions <- s
	}
	return firstErr
}

func (p *goStylePool) releaseSession(s *maplibre.Session) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		_ = s.Close()
		return
	}
	p.sessions <- s
}

// close drains and disposes every Session in the pool. Renders blocked
// waiting on the channel will get cancelled by their own ctx; close
// itself does not cancel them.
func (p *goStylePool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	for {
		select {
		case s := <-p.sessions:
			_ = s.Close()
		default:
			return
		}
	}
}

// acquireBuf returns a buffer of len >= n from the pool, or allocates
// a fresh one. The pool stores *[]byte to keep escape analysis happy.
func (p *goStylePool) acquireBuf(n int) []byte {
	if v := p.bufPool.Get(); v != nil {
		b := *(v.(*[]byte))
		if cap(b) >= n {
			return b[:n]
		}
	}
	return make([]byte, n)
}

func (p *goStylePool) releaseBuf(b []byte) {
	if cap(b) == 0 {
		return
	}
	bp := b[:0]
	p.bufPool.Put(&bp)
}

// unpremultiplyRGBA converts premultiplied RGBA bytes (binding output)
// to non-premultiplied RGBA (image.NRGBA layout). Pure byte loop —
// hot enough on the render path that we keep the implementation
// inline rather than calling into image/color helpers.
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

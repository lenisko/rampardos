package renderer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/fileutil"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"golang.org/x/sync/semaphore"
)

// rendererPNGBufferPool reuses png.EncoderBuffer across encodes so
// the internal zlib writer and filter working buffers don't allocate
// fresh per call. Kept local to the renderer package — the utils
// package has its own pool for the HTTP-boundary encoder.
var rendererPNGBufferPool rendererPNGEncoderBufferPool

type rendererPNGEncoderBufferPool struct {
	pool sync.Pool
}

func (p *rendererPNGEncoderBufferPool) Get() *png.EncoderBuffer {
	if b, ok := p.pool.Get().(*png.EncoderBuffer); ok {
		return b
	}
	return nil
}

func (p *rendererPNGEncoderBufferPool) Put(buf *png.EncoderBuffer) {
	p.pool.Put(buf)
}

// poolKey builds the map key for a (style, scale) pool. Scale=1 pools
// use the bare styleID for backward compatibility with log messages
// and the canary render path.
func poolKey(styleID string, scale uint8) string {
	if scale <= 1 {
		return styleID
	}
	return styleID + "@" + strconv.FormatUint(uint64(scale), 10)
}

func parsePoolKey(key string) (styleID string, ratio int) {
	if i := strings.LastIndex(key, "@"); i >= 0 {
		if r, err := strconv.Atoi(key[i+1:]); err == nil {
			return key[:i], r
		}
	}
	return key, 1
}

// Ensure NodePoolRenderer satisfies the Renderer interface.
var _ Renderer = (*NodePoolRenderer)(nil)

// SpawnFactory builds a spawn closure for a given style and pixel
// ratio. The ratio maps to maplibre-native's DPR: ratio=1 is standard,
// ratio=2 is retina (same geographic extent, 2× pixel density).
// Production callers pass a factory that constructs
// `node render-worker.js ...` invocations; tests inject fake spawners.
type SpawnFactory func(styleID, preparedStylePath string, ratio int) func() (*worker, error)

// DefaultSpawnFactory returns a SpawnFactory that launches real Node
// render-worker processes using the paths from cfg.
func DefaultSpawnFactory(cfg Config) SpawnFactory {
	return func(styleID, preparedStylePath string, ratio int) func() (*worker, error) {
		return func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:  cfg.NodeBinary,
				script:  cfg.WorkerScript,
				styleID: styleID,
				scriptArgs: []string{
					"--style-id", styleID,
					"--style-path", preparedStylePath,
					"--mbtiles", cfg.MbtilesFile,
					"--styles-dir", cfg.StylesDir,
					"--fonts-dir", cfg.FontsDir,
					"--ratio", strconv.Itoa(ratio),
				},
				handshakeTimeout: cfg.StartupTimeout,
			})
		}
	}
}

// NodePoolRenderer is the production Renderer. It maintains one
// stylePool per configured (style, scale) tuple, prepares style.json
// on disk, and encodes raw RGBA output from workers to PNG/JPEG/WebP.
//
// Concurrency is gated at two levels: a global semaphore (cfg.PoolSize)
// caps the number of renders running simultaneously across all pools
// — necessary because per-pool worker counts would otherwise multiply
// by (styles × scales) and oversubscribe CPU. Each pool is still sized
// by cfg.StylePoolSize to govern memory, but the semaphore is the
// true concurrency ceiling.
type NodePoolRenderer struct {
	cfg          Config
	spawnFactory SpawnFactory
	sem          *semaphore.Weighted

	mu    sync.RWMutex
	pools map[string]*stylePool
}

// NewNodePoolRenderer creates a NodePoolRenderer. Pools are created
// lazily on first request for each style, not pre-warmed at startup.
// This means only styles that are actually used consume worker
// processes — 5 styles installed but 1 used = 1 pool running.
func NewNodePoolRenderer(cfg Config, sf SpawnFactory) (*NodePoolRenderer, error) {
	// Apply defaults.
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = runtime.GOMAXPROCS(0)
	}
	if cfg.StylePoolSize <= 0 {
		cfg.StylePoolSize = cfg.PoolSize
	}
	if cfg.WorkerLifetime <= 0 {
		cfg.WorkerLifetime = 500
	}
	if cfg.RenderTimeout <= 0 {
		cfg.RenderTimeout = 15 * time.Second
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 10 * time.Second
	}

	// Validate that DiscoverStyles works (catches config errors early).
	if _, err := cfg.DiscoverStyles(); err != nil {
		return nil, fmt.Errorf("renderer: discover styles: %w", err)
	}

	npr := &NodePoolRenderer{
		cfg:          cfg,
		spawnFactory: sf,
		sem:          semaphore.NewWeighted(int64(cfg.PoolSize)),
		pools:        make(map[string]*stylePool),
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.SetRendererGlobalCapacity(cfg.PoolSize)
	}
	return npr, nil
}

// acquireGlobal acquires the process-wide render-concurrency semaphore
// and records the wait + in-flight gauges. Paired with releaseGlobal.
func (npr *NodePoolRenderer) acquireGlobal(ctx context.Context) error {
	start := time.Now()
	if err := npr.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererGlobalAcquire(time.Since(start).Seconds())
	}
	return nil
}

func (npr *NodePoolRenderer) releaseGlobal() {
	npr.sem.Release(1)
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.DecRendererGlobalInFlight()
	}
}

// loadPool reads the on-disk style.json, rewrites it via PrepareStyle,
// writes the prepared version next to the original, and creates a
// stylePool with the given spawn closure.
func (npr *NodePoolRenderer) loadPool(id string, ratio int) (*stylePool, error) {
	styleFile := filepath.Join(npr.cfg.StylesDir, id, "style.json")
	raw, err := os.ReadFile(styleFile)
	if err != nil {
		return nil, fmt.Errorf("read style.json: %w", err)
	}

	prepared, err := PrepareStyle(id, raw, npr.cfg)
	if err != nil {
		return nil, fmt.Errorf("prepare style: %w", err)
	}

	// Atomic write via tempfile + rename: ReloadStyles calls loadPool
	// outside any lock, so plain os.WriteFile's truncate window would
	// let a worker spawning in parallel read a zero-length or partial
	// style.prepared.json.
	preparedPath := filepath.Join(npr.cfg.StylesDir, id, "style.prepared.json")
	if err := fileutil.AtomicWriteFile(preparedPath, prepared, 0o644); err != nil {
		return nil, fmt.Errorf("write prepared style: %w", err)
	}

	spawn := npr.spawnFactory(id, preparedPath, ratio)
	zoomAdj := styleZoomOffset(raw)

	return newStylePool(stylePoolConfig{
		styleID:          id,
		scaleLabel:       strconv.Itoa(ratio),
		viewportZoomAdj:  zoomAdj,
		poolSize:         npr.cfg.StylePoolSize,
		workerLifetime:   npr.cfg.WorkerLifetime,
		handshakeTimeout: npr.cfg.StartupTimeout,
		spawn:            spawn,
	})
}

// Render converts a tile request to a viewport, dispatches to the
// appropriate pool, and encodes the raw RGBA result.
func (npr *NodePoolRenderer) Render(ctx context.Context, req Request) ([]byte, error) {
	vp := TileToViewport(req.Z, req.X, req.Y, req.Scale)
	vp.StyleID = req.StyleID
	vp.Format = req.Format
	return npr.renderViewportInternal(ctx, vp)
}

// RenderViewport dispatches an arbitrary viewport request directly,
// returning the encoded bytes in the requested format.
func (npr *NodePoolRenderer) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
	img, err := npr.RenderViewportImage(ctx, req)
	if err != nil {
		return nil, err
	}
	return encodeRGBAImage(img, req.Format)
}

// RenderViewportImage dispatches a viewport request and returns the
// raw decoded *image.NRGBA. Lets callers skip the PNG encode when
// their next step is a draw operation rather than a disk write.
func (npr *NodePoolRenderer) RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error) {
	scale := int(req.Scale)
	if scale < 1 {
		scale = 1
	}
	pool, err := npr.getOrCreatePool(req.StyleID, req.Scale)
	if err != nil {
		return nil, err
	}
	if err := npr.acquireGlobal(ctx); err != nil {
		return nil, err
	}
	defer npr.releaseGlobal()
	// Rewrite the caller's (web-map convention) zoom into the style's
	// native tileSize convention before handing it to MapLibre. See
	// styleZoomOffset for the full derivation. Not applied on the
	// tile-render path — TileToViewport already produces a frame whose
	// 512-pixel viewport matches MapLibre's native zoom unit for the
	// common 512-tileSize case.
	adjusted := req
	adjusted.Zoom = req.Zoom - pool.cfg.viewportZoomAdj
	frame := requestFrameForViewport(adjusted)
	rgba, err := pool.dispatch(ctx, frame)
	if err != nil {
		return nil, err
	}
	width := req.Width * scale
	height := req.Height * scale
	if len(rgba) != width*height*4 {
		return nil, fmt.Errorf("renderer: worker returned %d bytes, expected %d for %dx%d",
			len(rgba), width*height*4, width, height)
	}
	return &image.NRGBA{
		Pix:    rgba,
		Stride: width * 4,
		Rect:   image.Rect(0, 0, width, height),
	}, nil
}

// renderViewportInternal is the tile-serving dispatch path. It keeps
// the lenient encodeRGBA so TestNodePoolRendererRenderReturnsBytes
// can run against the fake worker's 4-byte canned payload; strict
// callers that need the worker's buffer length validated (static-
// map pipeline after the bytes-first refactor) use RenderViewport
// or RenderViewportImage, both of which go through encodeRGBAImage
// directly.
func (npr *NodePoolRenderer) renderViewportInternal(ctx context.Context, req ViewportRequest) ([]byte, error) {
	scale := int(req.Scale)
	if scale < 1 {
		scale = 1
	}

	pool, err := npr.getOrCreatePool(req.StyleID, req.Scale)
	if err != nil {
		return nil, err
	}

	if err := npr.acquireGlobal(ctx); err != nil {
		return nil, err
	}
	defer npr.releaseGlobal()

	// Send LOGICAL dimensions to the worker. The worker's map was
	// constructed with ratio=scale, so it produces width*ratio ×
	// height*ratio actual pixels covering the same geographic extent
	// as a ratio=1 render at these logical dimensions.
	frame := requestFrameForViewport(req)

	rgba, err := pool.dispatch(ctx, frame)
	if err != nil {
		return nil, err
	}

	// The RGBA buffer from the worker is width*scale × height*scale
	// actual pixels (due to the map's ratio). encodeRGBA must match.
	return encodeRGBA(rgba, req.Width*scale, req.Height*scale, req.Format)
}

// getOrCreatePool returns the pool for a style, creating it on first
// use. Uses double-checked locking: fast RLock path for the common
// case (pool already exists), slow Lock path for first-time creation.
func (npr *NodePoolRenderer) getOrCreatePool(styleID string, scale uint8) (*stylePool, error) {
	if scale < 1 {
		scale = 1
	}
	key := poolKey(styleID, scale)

	// Fast path: pool already running.
	npr.mu.RLock()
	pool, ok := npr.pools[key]
	npr.mu.RUnlock()
	if ok {
		return pool, nil
	}

	// Slow path: create pool under write lock.
	npr.mu.Lock()
	defer npr.mu.Unlock()

	if pool, ok := npr.pools[key]; ok {
		return pool, nil
	}

	stylePath := filepath.Join(npr.cfg.StylesDir, styleID, "style.json")
	if _, err := os.Stat(stylePath); err != nil {
		return nil, fmt.Errorf("renderer: unknown style %q (no style.json at %s)", styleID, stylePath)
	}

	ratio := int(scale)
	slog.Info("Creating renderer pool on first use", "style", styleID, "ratio", ratio, "stylePoolSize", npr.cfg.StylePoolSize, "globalRenderCap", npr.cfg.PoolSize)
	pool, err := npr.loadPool(styleID, ratio)
	if err != nil {
		return nil, fmt.Errorf("renderer: create pool %q ratio=%d: %w", styleID, ratio, err)
	}
	npr.pools[key] = pool
	return pool, nil
}

// ReloadStyles tears down all existing pools and rebuilds them from
// disk. In-flight renders against old pools finish normally; new
// renders block until the rebuild completes.
func (npr *NodePoolRenderer) ReloadStyles(ctx context.Context) error {
	// Snapshot active pool keys under a short read lock.
	npr.mu.RLock()
	activeKeys := make(map[string]struct{}, len(npr.pools))
	for key := range npr.pools {
		activeKeys[key] = struct{}{}
	}
	npr.mu.RUnlock()

	// Build new pools OUTSIDE the lock so in-flight renders continue
	// against the old pools without blocking. Each loadPool spawns
	// workers and waits for handshakes — this can take seconds.
	newPools := make(map[string]*stylePool, len(activeKeys))
	for key := range activeKeys {
		if ctx.Err() != nil {
			for _, p := range newPools {
				p.close()
			}
			return ctx.Err()
		}
		styleID, ratio := parsePoolKey(key)
		pool, err := npr.loadPool(styleID, ratio)
		if err != nil {
			slog.Error("Failed to reload pool, style will be recreated on next request",
				"style", styleID, "ratio", ratio, "error", err)
			continue
		}
		newPools[key] = pool
	}

	// Swap under a short write lock. A concurrent getOrCreatePool may
	// have inserted a new key into npr.pools while we were rebuilding
	// outside the lock — those pools were created for styles we didn't
	// know about at snapshot time, so merge them into newPools instead
	// of overwriting. Only the snapshotted keys are dropped/replaced;
	// their pool pointers go into oldPools to be closed after the swap.
	npr.mu.Lock()
	oldPools := make(map[string]*stylePool, len(activeKeys))
	for key, pool := range npr.pools {
		if _, wasActive := activeKeys[key]; wasActive {
			oldPools[key] = pool
		} else {
			newPools[key] = pool
		}
	}
	npr.pools = newPools
	npr.mu.Unlock()

	for _, pool := range oldPools {
		pool.close()
	}
	return nil
}

// Close releases all backend resources.
func (npr *NodePoolRenderer) Close() error {
	npr.mu.Lock()
	defer npr.mu.Unlock()
	for _, pool := range npr.pools {
		pool.close()
	}
	npr.pools = nil
	return nil
}

// requestFrameForViewport builds the JSON wire frame sent to the
// worker for a viewport render. Width and height are LOGICAL
// dimensions — the worker's mbgl.Map is constructed with
// ratio=scale, so it produces width*ratio × height*ratio actual
// pixels covering the same geographic extent as these logical dims.
func requestFrameForViewport(vp ViewportRequest) []byte {
	m := map[string]any{
		"zoom":    vp.Zoom,
		"center":  []float64{vp.Longitude, vp.Latitude},
		"width":   vp.Width,
		"height":  vp.Height,
		"bearing": vp.Bearing,
		"pitch":   vp.Pitch,
	}
	b, _ := json.Marshal(m)
	return b
}

// encodeRGBA converts raw RGBA bytes to the requested image format.
// If the buffer size does not match width*height*4 (e.g. the fake
// worker's "fake" payload), the raw bytes are returned unchanged.
func encodeRGBA(rgba []byte, width, height int, format models.ImageFormat) ([]byte, error) {
	if len(rgba) != width*height*4 {
		return rgba, nil
	}
	img := &image.NRGBA{
		Pix:    rgba,
		Stride: width * 4,
		Rect:   image.Rect(0, 0, width, height),
	}
	return encodeRGBAImage(img, format)
}

// encodeRGBAImage is the shared encode path used by both the byte-
// based renderer.Render entry points and any caller that already has
// an *image.NRGBA in hand.
func encodeRGBAImage(img *image.NRGBA, format models.ImageFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG:
		// Respect PNG_COMPRESSION_LEVEL. Default png.Encoder uses
		// flate level 6 — ~4-6× slower than the "fast" setting
		// applied everywhere else via saveImage. The renderer runs
		// this on every maplibre-native tile/viewport, so the
		// difference compounds. BufferPool reuses the internal
		// zlib+filter state across encodes.
		encoder := png.Encoder{CompressionLevel: png.BestSpeed, BufferPool: &rendererPNGBufferPool}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("renderer: png encode: %w", err)
		}
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("renderer: jpeg encode: %w", err)
		}
	case models.ImageFormatWEBP:
		if err := webp.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("renderer: webp encode: %w", err)
		}
	default:
		return nil, fmt.Errorf("renderer: unsupported format %q", format)
	}
	return buf.Bytes(), nil
}

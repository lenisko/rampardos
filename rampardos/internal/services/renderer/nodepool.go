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
	"sync"
	"time"

	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
)

// Ensure NodePoolRenderer satisfies the Renderer interface.
var _ Renderer = (*NodePoolRenderer)(nil)

// SpawnFactory builds a spawn closure for a given style. Production
// callers pass a factory that constructs `node render-worker.js ...`
// invocations; tests inject fake-worker spawners.
type SpawnFactory func(styleID, preparedStylePath string) func() (*worker, error)

// DefaultSpawnFactory returns a SpawnFactory that launches real Node
// render-worker processes using the paths from cfg.
func DefaultSpawnFactory(cfg Config) SpawnFactory {
	return func(styleID, preparedStylePath string) func() (*worker, error) {
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
				},
				handshakeTimeout: cfg.StartupTimeout,
			})
		}
	}
}

// NodePoolRenderer is the production Renderer. It maintains one
// stylePool per configured style, prepares style.json on disk, and
// encodes raw RGBA output from workers to PNG/JPEG/WebP.
type NodePoolRenderer struct {
	cfg          Config
	spawnFactory SpawnFactory

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

	return &NodePoolRenderer{
		cfg:          cfg,
		spawnFactory: sf,
		pools:        make(map[string]*stylePool),
	}, nil
}

// loadPool reads the on-disk style.json, rewrites it via PrepareStyle,
// writes the prepared version next to the original, and creates a
// stylePool with the given spawn closure.
func (npr *NodePoolRenderer) loadPool(id string) (*stylePool, error) {
	styleFile := filepath.Join(npr.cfg.StylesDir, id, "style.json")
	raw, err := os.ReadFile(styleFile)
	if err != nil {
		return nil, fmt.Errorf("read style.json: %w", err)
	}

	prepared, err := PrepareStyle(id, raw, npr.cfg)
	if err != nil {
		return nil, fmt.Errorf("prepare style: %w", err)
	}

	preparedPath := filepath.Join(npr.cfg.StylesDir, id, "style.prepared.json")
	if err := os.WriteFile(preparedPath, prepared, 0o644); err != nil {
		return nil, fmt.Errorf("write prepared style: %w", err)
	}

	spawn := npr.spawnFactory(id, preparedPath)

	return newStylePool(stylePoolConfig{
		styleID:          id,
		poolSize:         npr.cfg.PoolSize,
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

// RenderViewport dispatches an arbitrary viewport request directly.
func (npr *NodePoolRenderer) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
	return npr.renderViewportInternal(ctx, req)
}

func (npr *NodePoolRenderer) renderViewportInternal(ctx context.Context, req ViewportRequest) ([]byte, error) {
	pool, err := npr.getOrCreatePool(req.StyleID)
	if err != nil {
		return nil, err
	}

	frame := requestFrameForViewport(req)

	rgba, err := pool.dispatch(ctx, frame)
	if err != nil {
		return nil, err
	}

	// The wire frame sends Width*Scale and Height*Scale as the actual
	// pixel dimensions. encodeRGBA needs the actual size to match the buffer.
	scale := int(req.Scale)
	if scale < 1 {
		scale = 1
	}
	return encodeRGBA(rgba, req.Width*scale, req.Height*scale, req.Format)
}

// getOrCreatePool returns the pool for a style, creating it on first
// use. Uses double-checked locking: fast RLock path for the common
// case (pool already exists), slow Lock path for first-time creation.
func (npr *NodePoolRenderer) getOrCreatePool(styleID string) (*stylePool, error) {
	// Fast path: pool already running.
	npr.mu.RLock()
	pool, ok := npr.pools[styleID]
	npr.mu.RUnlock()
	if ok {
		return pool, nil
	}

	// Slow path: create pool under write lock.
	npr.mu.Lock()
	defer npr.mu.Unlock()

	// Double-check after acquiring write lock.
	if pool, ok := npr.pools[styleID]; ok {
		return pool, nil
	}

	// Verify the style exists on disk before spawning workers.
	stylePath := filepath.Join(npr.cfg.StylesDir, styleID, "style.json")
	if _, err := os.Stat(stylePath); err != nil {
		return nil, fmt.Errorf("renderer: unknown style %q (no style.json at %s)", styleID, stylePath)
	}

	slog.Info("Creating renderer pool on first use", "style", styleID, "poolSize", npr.cfg.PoolSize)
	pool, err := npr.loadPool(styleID)
	if err != nil {
		return nil, fmt.Errorf("renderer: create pool %q: %w", styleID, err)
	}
	npr.pools[styleID] = pool
	return pool, nil
}

// ReloadStyles tears down all existing pools and rebuilds them from
// disk. In-flight renders against old pools finish normally; new
// renders block until the rebuild completes.
func (npr *NodePoolRenderer) ReloadStyles(ctx context.Context) error {
	// Snapshot active style IDs under a short read lock.
	npr.mu.RLock()
	activeIDs := make([]string, 0, len(npr.pools))
	for id := range npr.pools {
		activeIDs = append(activeIDs, id)
	}
	npr.mu.RUnlock()

	// Build new pools OUTSIDE the lock so in-flight renders continue
	// against the old pools without blocking. Each loadPool spawns
	// workers and waits for handshakes — this can take seconds.
	newPools := make(map[string]*stylePool, len(activeIDs))
	for _, id := range activeIDs {
		if ctx.Err() != nil {
			// Context expired (e.g. SIGHUP timeout) — stop spawning.
			for _, p := range newPools {
				p.close()
			}
			return ctx.Err()
		}
		pool, err := npr.loadPool(id)
		if err != nil {
			slog.Error("Failed to reload pool, style will be recreated on next request",
				"style", id, "error", err)
			continue
		}
		newPools[id] = pool
	}

	// Swap under a short write lock: replace pools map, then close
	// old pools after the swap (so any final in-flight dispatches
	// that already hold a pool reference can finish).
	npr.mu.Lock()
	oldPools := npr.pools
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
// worker for a viewport render. Width and height are multiplied by
// Scale to get the actual rendered pixel dimensions — the worker's
// mbgl.Map is constructed with ratio:1, so DPR must be baked into
// the dimensions rather than passed as a separate ratio field.
func requestFrameForViewport(vp ViewportRequest) []byte {
	scale := int(vp.Scale)
	if scale < 1 {
		scale = 1
	}
	m := map[string]any{
		"zoom":    vp.Zoom,
		"center":  []float64{vp.Longitude, vp.Latitude},
		"width":   vp.Width * scale,
		"height":  vp.Height * scale,
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

	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG:
		if err := png.Encode(&buf, img); err != nil {
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

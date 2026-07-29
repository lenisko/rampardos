// Package renderer rasterises vector mbtiles into encoded image bytes.
// The implementation is the in-process Go renderer (gopool.go), driving
// maplibre-native through its C ABI. The Renderer interface is kept
// backend-agnostic so an alternative implementation could replace it
// without touching call sites.
package renderer

import (
	"context"
	"image"
	"time"

	"github.com/lenisko/rampardos/internal/models"
)

// Renderer produces encoded image bytes for tiles and viewports.
type Renderer interface {
	// Render produces encoded image bytes for a single tile on the
	// standard XYZ grid. Callers use this for the cacheable hot path:
	// integer zoom, grid-aligned coordinates, no rotation or tilt.
	// The implementation owns encoding (png/jpg/webp) and returns
	// ready-to-serve bytes.
	// Timeouts and cancellation are communicated via ctx.
	Render(ctx context.Context, req Request) ([]byte, error)

	// RenderViewport produces encoded image bytes for an arbitrary
	// map viewport. Callers use this when the request cannot be
	// satisfied from integer-coord tiles: fractional zoom, non-zero
	// bearing, non-zero pitch, non-standard dimensions. Bypasses
	// any tile-level cache; the caller is responsible for any
	// higher-level caching.
	RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error)

	// RenderViewportImage is like RenderViewport but returns the
	// decoded image.Image directly, avoiding a round-trip through
	// encoded bytes. Callers that intend to composite the result
	// in memory (e.g. the static-map overlay step) should prefer
	// this over RenderViewport + image.Decode.
	RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error)

	// ReloadStyles tears down and rebuilds backend state so that
	// subsequent renders see the latest on-disk style/mbtiles content.
	// Invoked from the dataset-refresh reload callback. Blocks until
	// the rebuild is complete. In-flight renders finish against the
	// old state; new renders queue until the rebuild is done.
	ReloadStyles(ctx context.Context) error

	// Close releases all backend resources. Safe to call multiple times.
	Close() error
}

// Request is a tile on the standard XYZ grid.
type Request struct {
	StyleID string
	Z, X, Y int
	Scale   uint8 // DPR / pixel ratio; typically 1 or 2. Implementations may reject larger values.
	Format  models.ImageFormat
}

// ViewportRequest is an arbitrary map view.
type ViewportRequest struct {
	StyleID   string
	Longitude float64
	Latitude  float64
	Zoom      float64 // may be fractional
	Width     int     // logical pixels (multiplied by Scale for actual rendered size)
	Height    int
	Bearing   float64 // degrees counter-clockwise from north; 0 for no rotation
	Pitch     float64 // degrees; 0 for flat
	Scale     uint8   // DPR / pixel ratio; typically 1 or 2. Implementations may reject larger values.
	Format    models.ImageFormat
}

// Config selects and parameterises a Renderer backend.
type Config struct {
	// Backend selects the implementation. Only "go-pool" exists; retained
	// so RENDERER_BACKEND stays a validated setting rather than silently
	// ignored.
	Backend string

	// Two-level concurrency. PoolSize is a process-global semaphore
	// capping concurrent renders across every (style, scale) pool;
	// StylePoolSize caps the workers within one pool. Raising StylePoolSize
	// above PoolSize cannot buy throughput — the global semaphore forbids
	// the extra concurrency — but it does raise the ceiling a single hot
	// pool can reach while others stay at their floor.
	PoolSize      int // global cap on concurrent renders (default: runtime.GOMAXPROCS(0))
	StylePoolSize int // ceiling on workers per (style, scale) pool (default: PoolSize)

	// StylePoolMin is the number of workers a pool keeps when idle
	// (default: 1). Pools start here and grow towards StylePoolSize when a
	// dispatch finds every worker busy, then retire one worker per
	// StylePoolIdleTTL of quiet back down to the floor. This matters
	// because pools are per (style, scale): a single static size
	// over-provisions every rarely-used combination while still capping
	// the busy one.
	StylePoolMin int

	// StylePoolIdleTTL is the quiet interval after which a pool retires
	// one worker (default: 2m). Growth is immediate; shrink is one worker
	// per interval so a brief lull doesn't collapse a hot pool.
	StylePoolIdleTTL time.Duration

	RenderTimeout  time.Duration // per-request deadline (default: 15s)
	StartupTimeout time.Duration // max time to wait for worker startup (default: 30s)

	// Asset paths resolved to absolute paths at load time.
	StylesDir   string // e.g. "TileServer/Styles"
	FontsDir    string // e.g. "TileServer/Fonts"
	MbtilesFile string // e.g. "TileServer/Datasets/Combined.mbtiles"

	// DiscoverStyles returns the current set of local style IDs by
	// scanning the disk. Called at startup and on each ReloadStyles
	// so that newly added style directories are picked up without a
	// server restart. Required.
	DiscoverStyles func() ([]string, error)
}

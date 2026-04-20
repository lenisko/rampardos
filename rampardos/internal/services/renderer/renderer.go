// Package renderer rasterises vector mbtiles into encoded image bytes.
// The Renderer interface is intentionally backend-agnostic: production
// uses a Node worker pool (see nodepool.go) loading
// @maplibre/maplibre-gl-native, but the interface is shaped so that a
// future source-built mbgl-render binary backend could replace it with
// no changes to call sites.
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
	Scale     uint8 // DPR / pixel ratio; typically 1 or 2. Implementations may reject larger values.
	Format    models.ImageFormat
}

// Config selects and parameterises a Renderer backend.
type Config struct {
	// Backend selects the implementation. Currently only "node-pool".
	Backend string

	// Node worker pool configuration (ignored by other backends).
	NodeBinary     string        // "node" if on PATH; else absolute path
	WorkerScript   string        // absolute path to render-worker.js
	WorkerModules  string        // absolute path to node_modules dir the worker should load from. Typically separate from WorkerScript — e.g. the script lives in rampardos/ and node_modules is installed under /app/render-worker/ at Docker build time.
	PoolSize       int           // global cap on concurrent renders across all pools (default: runtime.GOMAXPROCS(0)). Bounds CPU oversubscription when many (style, scale) pools exist.
	StylePoolSize  int           // workers per (style, scale) pool (default: PoolSize). Controls memory footprint per pool, not concurrent renders — the global PoolSize semaphore gates that.
	RenderTimeout  time.Duration // per-request deadline (default: 15s)
	WorkerLifetime int           // max renders per worker before recycling (default: 500)
	StartupTimeout time.Duration // max time to wait for a worker handshake (default: 10s)

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

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/services/renderer"
	"github.com/lenisko/rampardos/internal/utils"
	"golang.org/x/sync/singleflight"
)

// stylesControllerGetExternal is the subset of StylesController used by
// StaticMapHandler's dispatch logic. An interface keeps dispatch
// unit-testable without spinning up a real StylesController.
type stylesControllerGetExternal interface {
	GetExternalStyle(name string) *models.Style
}

// StaticMapHandler handles static map requests
type StaticMapHandler struct {
	renderer          renderer.Renderer
	tileHandler       *TileHandler
	statsController   *services.StatsController
	stylesController  stylesControllerGetExternal
	sphericalMercator *utils.SphericalMercator
	sfg               singleflight.Group // dedup concurrent generates for same final path
	baseSfg           singleflight.Group // dedup concurrent base renders for same basePath

	// localUseViewport routes local-style integer-zoom bases through
	// RenderViewport instead of tile stitching when the tile working
	// set is too large to benefit from the cache. Wired from
	// config.LocalStylesUseViewport.
	localUseViewport bool

	// Function-valued hooks. Production wiring in NewStaticMapHandler
	// sets these to the real methods below; tests override them to
	// record dispatch without touching the renderer or disk.
	generateBaseStaticMapFromAPIFn   func(ctx context.Context, sm models.StaticMap) (image.Image, error)
	generateBaseStaticMapFromTilesFn func(ctx context.Context, sm models.StaticMap, basePath string, extStyle *models.Style) (image.Image, error)
	logExternalViewportApproxFn      func(sm models.StaticMap)
}

// NewStaticMapHandler creates a new static map handler
func NewStaticMapHandler(r renderer.Renderer, tileHandler *TileHandler, statsController *services.StatsController, stylesController *services.StylesController, localUseViewport bool) *StaticMapHandler {
	h := &StaticMapHandler{
		renderer:          r,
		tileHandler:       tileHandler,
		statsController:   statsController,
		stylesController:  stylesController,
		sphericalMercator: utils.NewSphericalMercator(),
		localUseViewport:  localUseViewport,
	}
	h.generateBaseStaticMapFromAPIFn = h.generateBaseStaticMapFromAPI
	h.generateBaseStaticMapFromTilesFn = h.generateBaseStaticMapFromTiles
	h.logExternalViewportApproxFn = h.logExternalViewportApprox
	return h
}

// Get handles GET /staticmap
func (h *StaticMapHandler) Get(w http.ResponseWriter, r *http.Request) {
	var staticMap models.StaticMap
	if err := parseQueryParams(r, &staticMap); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, staticMap)
}

// Post handles POST /staticmap
func (h *StaticMapHandler) Post(w http.ResponseWriter, r *http.Request) {
	var staticMap models.StaticMap
	if err := json.NewDecoder(r.Body).Decode(&staticMap); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, staticMap)
}

// GetTemplate handles GET /staticmap/:template
func (h *StaticMapHandler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	template := chi.URLParam(r, "template")
	if template == "" {
		http.Error(w, "Missing template", http.StatusBadRequest)
		return
	}
	services.GlobalMetrics.RecordTemplateRender(template, "get", "staticmap")

	staticMap, err := h.renderTemplate(r.Context(), template, r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, *staticMap)
}

// PostTemplate handles POST /staticmap/:template
func (h *StaticMapHandler) PostTemplate(w http.ResponseWriter, r *http.Request) {
	template := chi.URLParam(r, "template")
	if template == "" {
		http.Error(w, "Missing template", http.StatusBadRequest)
		return
	}
	services.GlobalMetrics.RecordTemplateRender(template, "post", "staticmap")

	var context map[string]any
	if err := json.NewDecoder(r.Body).Decode(&context); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	staticMap, err := h.renderTemplateWithContext(r.Context(), template, context)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, *staticMap)
}

// GetPregenerated handles GET /staticmap/pregenerated/:id
func (h *StaticMapHandler) GetPregenerated(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" || strings.Contains(id, "..") {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	h.handlePregeneratedRequest(w, r, id)
}

// GenerateOpts controls caching behaviour for component map generation.
// In the bytes-first pipeline the in-memory LRU is always consulted;
// nocache semantics collapse to "use a short TTL" on the pregenerate
// disk-write path, which the caller expresses directly via TTL.
type GenerateOpts struct {
	// TTL governs disk-file deletion for pregenerate requests. Has
	// no effect on non-pregenerate paths — those never write a file.
	// Callers conveying the ?nocache=true semantic set this to
	// nocacheBaseTTLFloor (30 s); zero leaves pregenerate files under
	// the CacheCleaner's OwnedThreshold age sweep.
	TTL time.Duration
}

// GenerateStaticMap generates a static map and returns the decoded
// image. Used by MultiStaticMapHandler to skip the PNG round-trip
// that would otherwise happen between component generation and
// grid composition. Concurrent requests for the same map are
// deduplicated via singleflight; the sfg return value is the
// image pointer so followers share the leader's result without
// re-encoding.
//
// Expiry-queue enqueue has been moved out of this function. In the
// bytes-first pipeline nothing is written to disk on this path —
// persistence and its matching expiry enqueue now live exclusively
// in handlePregenerateResponseBytes, the sole disk-write site.
func (h *StaticMapHandler) GenerateStaticMap(ctx context.Context, staticMap models.StaticMap, opts GenerateOpts) (image.Image, error) {
	path := staticMap.Path()
	basePath := staticMap.BasePath()

	// NoCache governs disk-file lifetime at the pregenerate layer, not
	// in-memory reuse. Weather-alert fan-out hits this path with
	// nocache=true and the whole point is that N siblings share the
	// single render via the LRU.
	if services.GlobalCompositeImageCache != nil {
		if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
			return cached, nil
		}
	}

	// Detach the caller's cancellation from the shared render. singleflight
	// runs the callback once for all joiners; if the leader disconnects,
	// cancelling its ctx would abort generation for every follower that
	// is still live. Values (deadlines, trace IDs) still flow through.
	genCtx := context.WithoutCancel(ctx)
	v, err, _ := h.sfg.Do(path, func() (any, error) {
		if services.GlobalCompositeImageCache != nil {
			if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
				return cached, nil
			}
		}
		img, err := h.generateStaticMap(genCtx, path, basePath, staticMap)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(path, img)
		}
		return img, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(image.Image), nil
}

func (h *StaticMapHandler) handleRequest(w http.ResponseWriter, r *http.Request, staticMap models.StaticMap) {
	// Validate required fields
	if staticMap.Zoom < 1 || staticMap.Zoom > 22 {
		services.GlobalMetrics.RecordError("staticmap", "invalid_zoom")
		http.Error(w, "Invalid zoom level (must be 1-22)", http.StatusBadRequest)
		return
	}
	if staticMap.Width == 0 || staticMap.Height == 0 {
		services.GlobalMetrics.RecordError("staticmap", "invalid_dimensions")
		http.Error(w, "Invalid dimensions", http.StatusBadRequest)
		return
	}
	if staticMap.Style == "" {
		services.GlobalMetrics.RecordError("staticmap", "missing_style")
		http.Error(w, "Missing style", http.StatusBadRequest)
		return
	}

	path := staticMap.Path()
	basePath := staticMap.BasePath()
	startTime := time.Now()

	services.GlobalMetrics.IncrementInFlight("staticmap")
	defer services.GlobalMetrics.DecrementInFlight("staticmap")

	// Cache control:
	//   nocache=true — skip cache, serve image directly, delete immediately
	//   ttl=N        — keep file for N seconds then delete (e.g. Telegram)
	//   pregenerate=true + nocache=true — treated as ttl=30 so the file
	//     lives long enough for the consumer to fetch via the returned URL
	skipCache := r.URL.Query().Get("nocache") == "true"
	pregenerate := r.URL.Query().Get("pregenerate") == "true"
	ttlStr := r.URL.Query().Get("ttl")
	var ttlSeconds int
	if ttlStr != "" {
		ttlSeconds, _ = strconv.Atoi(ttlStr)
	}
	if skipCache && pregenerate {
		// Can't delete immediately if we're returning a filename — the
		// consumer needs time to fetch it. Convert to a short TTL.
		skipCache = false
		if ttlSeconds == 0 {
			ttlSeconds = 30
		}
	}

	// Deduplicate concurrent requests for the same static map via
	// singleflight. Two poracle webhooks for the same spawn arriving
	// simultaneously will only generate once. Detach the caller's
	// cancellation so a leader disconnect does not abort the shared
	// render for concurrent followers.
	genCtx := context.WithoutCancel(r.Context())
	v, genErr, _ := h.sfg.Do(path, func() (any, error) {
		// skipCache (from ?nocache=true) governs disk lifetime in the
		// pregenerate path, not in-memory reuse. Concurrent siblings
		// always benefit from the LRU.
		if services.GlobalCompositeImageCache != nil {
			if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
				return cached, nil
			}
		}
		img, err := h.generateStaticMap(genCtx, path, basePath, staticMap)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(path, img)
		}
		return img, nil
	})

	if genErr != nil {
		slog.Error("Failed to generate static map", "error", genErr)
		services.GlobalMetrics.RecordError("staticmap", "generation_failed")
		http.Error(w, genErr.Error(), http.StatusInternalServerError)
		return
	}
	img := v.(image.Image)
	encoded, err := utils.EncodeImage(img, filepath.Ext(path))
	if err != nil {
		slog.Error("Failed to encode static map", "error", err)
		services.GlobalMetrics.RecordError("staticmap", "encode_failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	duration := time.Since(startTime).Seconds()
	h.statsController.StaticMapServed(true, path, staticMap.Style)
	services.GlobalMetrics.RecordRequest("staticmap", staticMap.Style, false, duration)

	// ttl only has effect if pregenerate=true (handlePregenerateResponseBytes
	// writes the file + enqueues it); otherwise the response is served
	// from in-memory bytes with no disk artefact to expire. The
	// nocache+pregenerate conversion above already folded nocache into
	// ttlSeconds=30 for the pregenerate case.
	ttl := effectiveTTL(ttlSeconds)

	slog.Debug("Served static map", "file", filepath.Base(path), "duration", duration, "ttl", ttlSeconds, "nocache", skipCache)
	h.generateResponse(w, r, staticMap, path, encoded, ttl, basePath)
}

func (h *StaticMapHandler) handlePregeneratedRequest(w http.ResponseWriter, r *http.Request, id string) {
	path := fmt.Sprintf("Cache/Static/%s", id)
	regeneratablePath := fmt.Sprintf("Cache/Regeneratable/%s.json", filepath.Base(path))

	// Check if exists
	if _, err := os.Stat(path); err == nil {
		slog.Debug("Served static map (pregenerated)", "file", filepath.Base(path))
		serveFile(w, r, path)
		return
	}

	// Check for regeneratable
	if _, err := os.Stat(regeneratablePath); os.IsNotExist(err) {
		http.Error(w, "No regeneratable found with this id", http.StatusNotFound)
		return
	}

	// Load and regenerate
	data, err := os.ReadFile(regeneratablePath)
	if err != nil {
		http.Error(w, "Failed to read regeneratable", http.StatusInternalServerError)
		return
	}

	var staticMap models.StaticMap
	if err := json.Unmarshal(data, &staticMap); err != nil {
		http.Error(w, "Failed to parse regeneratable", http.StatusInternalServerError)
		return
	}

	h.handleRequest(w, r, staticMap)
}

// ensureBase renders basePath if it's missing. Concurrent callers for
// the same basePath are deduplicated via baseSfg, so sibling requests
// (same viewport, different overlays) do not duplicate base rendering.
//
// LRU fast path: GlobalCompositeImageCache is checked before the
// singleflight so burst-sharing across requests avoids the disk stat.
//
// Singleflight key correctness: basePath == BasePath() ==
// hash(WithoutDrawables()), so two callers share a slot iff their
// base-rendering inputs (style/lat/lon/zoom/w/h/scale/bearing/pitch/
// format) are identical. Base generation reads none of Markers,
// Polygons, or Circles, so merging the renders is safe.
func (h *StaticMapHandler) ensureBase(ctx context.Context, staticMap models.StaticMap, basePath string) (image.Image, error) {
	// LRU fast path — covers the cross-request burst-sharing case
	// that the short-TTL disk cache used to handle.
	if services.GlobalCompositeImageCache != nil {
		if img, ok := services.GlobalCompositeImageCache.Get(basePath); ok {
			return img, nil
		}
	}

	v, err, _ := h.baseSfg.Do(basePath, func() (any, error) {
		if services.GlobalCompositeImageCache != nil {
			if img, ok := services.GlobalCompositeImageCache.Get(basePath); ok {
				return img, nil
			}
		}
		img, err := h.generateBaseStaticMap(ctx, staticMap, basePath)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(basePath, img)
		}
		return img, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(image.Image), nil
}

func (h *StaticMapHandler) generateStaticMap(ctx context.Context, path, basePath string, staticMap models.StaticMap) (image.Image, error) {
	baseImg, err := h.ensureBase(ctx, staticMap, basePath)
	if err != nil {
		return nil, err
	}

	hasDrawables := len(staticMap.Markers) > 0 || len(staticMap.Polygons) > 0 || len(staticMap.Circles) > 0

	if !hasDrawables {
		return baseImg, nil
	}

	if err := h.downloadMarkers(ctx, staticMap); err != nil {
		return nil, err
	}
	return utils.GenerateStaticMapFromImage(staticMap, baseImg, h.sphericalMercator)
}

func (h *StaticMapHandler) generateBaseStaticMap(ctx context.Context, staticMap models.StaticMap, basePath string) (image.Image, error) {
	extStyle := h.stylesController.GetExternalStyle(staticMap.Style)

	if extStyle == nil {
		// Local style: fractional zoom → viewport render (native float zoom).
		// Integer zoom → tile stitching (cacheable via Cache/Tile), unless
		// localUseViewport is set to skip the tile pipeline entirely.
		if isFractional(staticMap.Zoom) || h.localUseViewport {
			return h.generateBaseStaticMapFromAPIFn(ctx, staticMap)
		}
		return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, basePath, extStyle)
	}

	if isFractional(staticMap.Zoom) {
		h.logExternalViewportApproxFn(staticMap)
	}
	return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, basePath, extStyle)
}

func (h *StaticMapHandler) logExternalViewportApprox(sm models.StaticMap) {
	slog.Warn("external style requested at fractional zoom; rendering at integer approximation",
		"style", sm.Style,
		"zoom", sm.Zoom,
		"lat", sm.Latitude,
		"lng", sm.Longitude,
	)
}

func (h *StaticMapHandler) generateBaseStaticMapFromAPI(ctx context.Context, staticMap models.StaticMap) (image.Image, error) {
	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	bearing := 0.0
	if staticMap.Bearing != nil {
		bearing = *staticMap.Bearing
	}
	pitch := 0.0
	if staticMap.Pitch != nil {
		pitch = *staticMap.Pitch
	}

	start := time.Now()
	img, err := h.renderer.RenderViewportImage(ctx, renderer.ViewportRequest{
		StyleID:   staticMap.Style,
		Longitude: staticMap.Longitude,
		Latitude:  staticMap.Latitude,
		Zoom:      staticMap.Zoom,
		Width:     int(staticMap.Width),
		Height:    int(staticMap.Height),
		Bearing:   bearing,
		Pitch:     pitch,
		Scale:     scale,
		Format:    staticMap.GetFormat(),
	})
	services.GlobalMetrics.RecordRendererViewport(staticMap.Style, time.Since(start).Seconds())
	if err != nil {
		return nil, fmt.Errorf("renderer viewport: %w", err)
	}
	return img, nil
}

func (h *StaticMapHandler) generateBaseStaticMapFromTiles(ctx context.Context, staticMap models.StaticMap, basePath string, extStyle *models.Style) (image.Image, error) {
	// Calculate tiles needed
	center := models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude}
	zoom := int(staticMap.Zoom)

	// Get center tile. xDelta/yDelta are pixel offsets within the tile
	// in SphericalMercator's 256-based coordinate grid.
	centerX, centerY, xDelta, yDelta := h.sphericalMercator.XY(center, zoom)

	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	// Tile pixel size depends on the source:
	// - Local styles rendered via maplibre-native: 512px (MapLibre's base tile size)
	// - External raster tiles: 256px (standard web tile size)
	tilePixels := 256
	if extStyle == nil {
		tilePixels = renderer.TileSizePx
	}

	// How many tiles we need to cover the requested viewport.
	tilesX := int(math.Ceil(float64(staticMap.Width)/float64(tilePixels)/float64(scale))) + 1
	tilesY := int(math.Ceil(float64(staticMap.Height)/float64(tilePixels)/float64(scale))) + 1

	// External styles may use scale-aware URL templates; local styles
	// have no URL template at all.
	hasScale := false
	if extStyle != nil {
		hasScale = strings.Contains(extStyle.URL, "{@scale}") || strings.Contains(extStyle.URL, "{scale}")
	}
	// Local styles at scale>1 produce tiles at tilePixels*scale
	// (e.g. 1024px for scale=2). The crop in GenerateBaseStaticMap
	// must multiply offset and output dimensions by scale — the same
	// path external scale-aware tiles use via hasScale.
	if extStyle == nil && scale > 1 {
		hasScale = true
	}

	// Generate tiles in parallel. Each tile is an independent download
	// or render; parallelising cuts wall-clock time from N*latency to
	// ~1*latency for external tile sources like Mapbox satellite.
	type tileSlot struct {
		index int
		path  string
		err   error
	}
	totalTiles := (2*(tilesX/2) + 1) * (2*(tilesY/2) + 1)
	tilePaths := make([]string, totalTiles)
	results := make(chan tileSlot, totalTiles)

	i := 0
	for dy := -tilesY / 2; dy <= tilesY/2; dy++ {
		for dx := -tilesX / 2; dx <= tilesX/2; dx++ {
			tileX := centerX + dx
			tileY := centerY + dy
			idx := i
			i++
			go func() {
				result, err := h.tileHandler.GenerateTile(ctx, staticMap.Style, zoom, tileX, tileY, scale, staticMap.GetFormat())
				if err != nil {
					results <- tileSlot{index: idx, err: err}
					return
				}
				results <- tileSlot{index: idx, path: result.Path}
			}()
		}
	}
	for range totalTiles {
		slot := <-results
		if slot.err != nil {
			return nil, fmt.Errorf("failed to generate tile: %w", slot.err)
		}
		tilePaths[slot.index] = slot.path
	}

	// Calculate offset: position of the map center in the combined tile grid.
	// xDelta/yDelta are in SphericalMercator's 256-based pixel grid.
	// Scale them to match the actual tile pixel size.
	deltaScale := tilePixels / 256
	offsetX := int(xDelta)*deltaScale + (tilesX/2)*tilePixels
	offsetY := int(yDelta)*deltaScale + (tilesY/2)*tilePixels

	// Create redownloader callback for corrupted tiles
	redownload := func(tilePath string) error {
		// Parse tile path: Cache/Tile/style-z-x-y-scale.format
		base := filepath.Base(tilePath)
		ext := filepath.Ext(base)
		name := strings.TrimSuffix(base, ext)
		parts := strings.Split(name, "-")
		if len(parts) < 5 {
			return fmt.Errorf("invalid tile path format: %s", tilePath)
		}
		tileY, _ := strconv.Atoi(parts[len(parts)-2])
		tileX, _ := strconv.Atoi(parts[len(parts)-3])
		tileZ, _ := strconv.Atoi(parts[len(parts)-4])
		tileScale, _ := strconv.ParseUint(parts[len(parts)-1], 10, 8)
		tileStyle := strings.Join(parts[:len(parts)-4], "-")
		tileFormat := models.ImageFormat(strings.TrimPrefix(ext, "."))

		_, err := h.tileHandler.GenerateTile(ctx, tileStyle, tileZ, tileX, tileY, uint8(tileScale), tileFormat)
		return err
	}

	// Combine tiles and write basePath to disk.
	if err := utils.GenerateBaseStaticMap(staticMap, tilePaths, basePath, offsetX, offsetY, hasScale, redownload); err != nil {
		return nil, err
	}

	// Read the written file back and decode so callers can draw on the
	// in-memory canvas directly. One extra decode is acceptable here —
	// this is the external-style cold path that writes basePath anyway.
	encoded, err := os.ReadFile(basePath)
	if err != nil {
		return nil, fmt.Errorf("read stitched base: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode stitched base: %w", err)
	}
	return img, nil
}

func (h *StaticMapHandler) downloadMarkers(ctx context.Context, staticMap models.StaticMap) error {
	// Ensure marker cache directory exists
	ensureDir("Cache/Marker")

	// Collect markers that need downloading
	type markerDownload struct {
		marker models.Marker
		path   string
		domain string
		format string
	}
	var toDownload []markerDownload

	for _, marker := range staticMap.Markers {
		if !strings.HasPrefix(marker.URL, "http://") && !strings.HasPrefix(marker.URL, "https://") {
			continue
		}

		hash := utils.PersistentHashString(marker.URL)
		parts := strings.Split(marker.URL, ".")
		format := "png"
		if len(parts) > 0 {
			format = parts[len(parts)-1]
		}
		path := fmt.Sprintf("Cache/Marker/%s.%s", hash, format)
		domain := extractDomain(marker.URL)

		// Already cached on disk?
		if _, err := os.Stat(path); err == nil {
			h.statsController.MarkerServed(false, path, domain)
			continue
		}

		toDownload = append(toDownload, markerDownload{marker, path, domain, format})
	}

	if len(toDownload) == 0 {
		return nil
	}

	// Download markers in parallel (limit concurrency to 10)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for _, md := range toDownload {
		wg.Add(1)
		go func(md markerDownload) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := services.DownloadFile(ctx, md.marker.URL, md.path, "", 0); err != nil {
				if md.marker.FallbackURL == "" {
					return
				}
				fallbackHash := utils.PersistentHashString(md.marker.FallbackURL)
				fallbackPath := fmt.Sprintf("Cache/Marker/%s.%s", fallbackHash, md.format)
				fallbackDomain := extractDomain(md.marker.FallbackURL)

				if _, err := os.Stat(fallbackPath); err == nil {
					h.statsController.MarkerServed(false, fallbackPath, fallbackDomain)
					return
				}
				if err := services.DownloadFile(ctx, md.marker.FallbackURL, fallbackPath, "", 0); err == nil {
					h.statsController.MarkerServed(true, fallbackPath, fallbackDomain)
				}
			} else {
				h.statsController.MarkerServed(true, md.path, md.domain)
			}
		}(md)
	}

	wg.Wait()
	return nil
}

func extractDomain(url string) string {
	// Extract domain from URL for stats tracking
	parts := strings.Split(url, "//")
	if len(parts) < 2 {
		return "?"
	}
	domainParts := strings.Split(parts[1], "/")
	if len(domainParts) > 0 {
		return domainParts[0]
	}
	return "?"
}

func (h *StaticMapHandler) generateResponse(w http.ResponseWriter, r *http.Request, staticMap models.StaticMap, path string, encoded []byte, ttl time.Duration, basePath string) {
	if handlePregenerateResponseBytes(w, r, path, staticMap, encoded, ttl, basePath) {
		return
	}
	serveStaticMapBytes(w, r, path, encoded)
}

// serveStaticMapBytes sets cache-control header and serves the encoded
// image bytes via http.ServeContent, enabling range requests and
// conditional GET (If-None-Match / If-Modified-Since).
func serveStaticMapBytes(w http.ResponseWriter, r *http.Request, path string, encoded []byte) {
	w.Header().Set("Cache-Control", "max-age=604800, must-revalidate")
	http.ServeContent(w, r, filepath.Base(path), time.Now(), bytes.NewReader(encoded))
}

// effectiveTTL resolves the ?ttl query-param into the duration that
// governs disk-file lifetime for pregenerate requests. Zero falls
// through to OwnedThreshold (CacheCleaner age-sweep). The nocache+
// pregenerate combo is rewritten to ttlSeconds=30 at the handler
// entry, so nocache-specific handling lives there, not here.
func effectiveTTL(ttlSeconds int) time.Duration {
	if ttlSeconds > 0 {
		return time.Duration(ttlSeconds) * time.Second
	}
	return services.OwnedThreshold
}

func (h *StaticMapHandler) renderTemplate(ctx context.Context, template string, params map[string][]string) (*models.StaticMap, error) {
	// Convert query params to context
	context := make(map[string]any)
	for k, v := range params {
		if len(v) == 1 {
			context[k] = v[0]
		} else {
			context[k] = v
		}
	}
	return h.renderTemplateWithContext(ctx, template, context)
}

func (h *StaticMapHandler) renderTemplateWithContext(_ context.Context, templateName string, ctx map[string]any) (*models.StaticMap, error) {
	var staticMap models.StaticMap
	if err := renderTemplateToStruct(templateName, ctx, &staticMap, "staticmap_template"); err != nil {
		return nil, err
	}
	return &staticMap, nil
}

// renderTemplateToStruct renders a Jet template and unmarshals the result into the target struct
func renderTemplateToStruct(templateName string, ctx map[string]any, target any, metricType string) error {
	// Render template with Jet (uses cached templates from memory)
	content, err := services.GlobalJetRenderer.Render(templateName, ctx)
	if err != nil {
		services.GlobalMetrics.RecordError(metricType, "render_failed")
		return fmt.Errorf("failed to render template: %w", err)
	}

	// Clean up JSON issues from template rendering
	content = utils.CleanJSONTrailingCommas(content)

	if err := json.Unmarshal([]byte(content), target); err != nil {
		slog.Debug("Failed to decode template", "template", templateName, "error", err, "content", content)
		services.GlobalMetrics.RecordError(metricType, "decode_failed")
		return fmt.Errorf("failed to parse template: %w", err)
	}

	slog.Debug("Rendered template", "template", templateName)
	return nil
}

func parseQueryParams(r *http.Request, staticMap *models.StaticMap) error {
	q := r.URL.Query()

	staticMap.Style = q.Get("style")
	if staticMap.Style == "" {
		return fmt.Errorf("missing style parameter")
	}

	lat, err := strconv.ParseFloat(q.Get("latitude"), 64)
	if err != nil {
		return fmt.Errorf("invalid latitude")
	}
	staticMap.Latitude = lat

	lon, err := strconv.ParseFloat(q.Get("longitude"), 64)
	if err != nil {
		return fmt.Errorf("invalid longitude")
	}
	staticMap.Longitude = lon

	zoom, err := strconv.ParseFloat(q.Get("zoom"), 64)
	if err != nil {
		return fmt.Errorf("invalid zoom")
	}
	staticMap.Zoom = zoom

	width, err := strconv.ParseUint(q.Get("width"), 10, 16)
	if err != nil {
		return fmt.Errorf("invalid width")
	}
	staticMap.Width = uint16(width)

	height, err := strconv.ParseUint(q.Get("height"), 10, 16)
	if err != nil {
		return fmt.Errorf("invalid height")
	}
	staticMap.Height = uint16(height)

	scale, err := strconv.ParseUint(q.Get("scale"), 10, 8)
	if err != nil {
		scale = 1
	}
	staticMap.Scale = uint8(scale)

	if formatStr := q.Get("format"); formatStr != "" {
		format := models.ImageFormat(formatStr)
		staticMap.Format = &format
	}

	// Parse markers from JSON if present
	if markersJSON := q.Get("markers"); markersJSON != "" {
		var markers []models.Marker
		if err := json.Unmarshal([]byte(markersJSON), &markers); err == nil {
			staticMap.Markers = markers
		}
	}

	return nil
}

// isFractional reports whether the given zoom is non-integer, with
// epsilon tolerance for floating-point noise. NaN and infinities are
// treated as non-fractional (they fall through to whatever the
// downstream code already handles — these should never occur in
// validated input).
func isFractional(zoom float64) bool {
	if math.IsNaN(zoom) || math.IsInf(zoom, 0) {
		return false
	}
	const epsilon = 1e-9
	_, frac := math.Modf(zoom)
	if frac < 0 {
		frac = -frac
	}
	return frac > epsilon && frac < 1-epsilon
}

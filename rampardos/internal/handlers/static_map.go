package handlers

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Function-valued hooks. Production wiring in NewStaticMapHandler
	// sets these to the real methods below; tests override them to
	// record dispatch without touching tileserver-gl or disk.
	generateBaseStaticMapFromAPIFn   func(ctx context.Context, sm models.StaticMap, basePath string) error
	generateBaseStaticMapFromTilesFn func(ctx context.Context, sm models.StaticMap, basePath string, extStyle *models.Style) error
	logExternalViewportApproxFn      func(sm models.StaticMap)
}

// NewStaticMapHandler creates a new static map handler
func NewStaticMapHandler(r renderer.Renderer, tileHandler *TileHandler, statsController *services.StatsController, stylesController *services.StylesController) *StaticMapHandler {
	h := &StaticMapHandler{
		renderer:          r,
		tileHandler:       tileHandler,
		statsController:   statsController,
		stylesController:  stylesController,
		sphericalMercator: utils.NewSphericalMercator(),
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
type GenerateOpts struct {
	NoCache    bool          // if true, delete files immediately after the caller is done
	TTL        time.Duration // if > 0, queue files for deletion after this duration
}

// GenerateStaticMap generates a static map (used by MultiStaticMapHandler).
// Concurrent requests for the same map are deduplicated via singleflight.
func (h *StaticMapHandler) GenerateStaticMap(ctx context.Context, staticMap models.StaticMap, opts GenerateOpts) error {
	path := staticMap.Path()
	basePath := staticMap.BasePath()

	if !opts.NoCache {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}

	_, err, _ := h.sfg.Do(path, func() (any, error) {
		if !opts.NoCache {
			if _, err := os.Stat(path); err == nil {
				return nil, nil
			}
		}
		return nil, h.generateStaticMap(ctx, path, basePath, staticMap)
	})
	if err != nil {
		return err
	}

	// Every generation enqueues its intent so the extend-only queue
	// resolves concurrent nocache vs TTL vs cached races correctly.
	if opts.NoCache {
		enqueueWithBase(services.GlobalExpiryQueue, nocacheBaseTTLFloor, path, basePath)
	} else if opts.TTL > 0 {
		enqueueWithBase(services.GlobalExpiryQueue, opts.TTL, path, basePath)
	} else {
		enqueueWithBase(services.GlobalExpiryQueue, services.OwnedThreshold, path, basePath)
	}

	return nil
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

	// Check if cached — os.Stat is authoritative. The prior cache
	// index could outlive its on-disk file and serve a stale "true"
	// that led to a 404 on the subsequent ServeFile.
	cached := false
	if !skipCache {
		if _, err := os.Stat(path); err == nil {
			cached = true
		}
	}

	if cached {
		duration := time.Since(startTime).Seconds()
		slog.Debug("Served static map (cached)", "file", filepath.Base(path))
		h.statsController.StaticMapServed(false, path, staticMap.Style)
		services.GlobalMetrics.RecordRequest("staticmap", staticMap.Style, true, duration)
		h.generateResponse(w, r, staticMap, path)
		return
	}

	// Deduplicate concurrent requests for the same static map via
	// singleflight. Two poracle webhooks for the same spawn arriving
	// simultaneously will only generate once.
	_, genErr, _ := h.sfg.Do(path, func() (any, error) {
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		}
		return nil, h.generateStaticMap(r.Context(), path, basePath, staticMap)
	})

	if genErr != nil {
		slog.Error("Failed to generate static map", "error", genErr)
		services.GlobalMetrics.RecordError("staticmap", "generation_failed")
		http.Error(w, genErr.Error(), http.StatusInternalServerError)
		return
	}

	duration := time.Since(startTime).Seconds()
	h.statsController.StaticMapServed(true, path, staticMap.Style)
	services.GlobalMetrics.RecordRequest("staticmap", staticMap.Style, false, duration)

	if skipCache {
		slog.Debug("Served static map (nocache)", "file", filepath.Base(path), "duration", duration)
		serveFile(w, r, path)
		// Enqueue with the burst-sharing floor instead of os.Remove.
		// A concurrent pregenerate+ttl request for the same hash may
		// have told its subscribers to fetch the file later — an
		// immediate delete would 404 them. The extend-only expiry
		// queue ensures the longer TTL wins.
		enqueueWithBase(services.GlobalExpiryQueue, nocacheBaseTTLFloor, path, basePath)
		return
	}

	slog.Debug("Served static map (generated)", "file", filepath.Base(path), "duration", duration, "ttl", ttlSeconds)
	h.generateResponse(w, r, staticMap, path)

	if ttlSeconds > 0 {
		enqueueWithBase(services.GlobalExpiryQueue, time.Duration(ttlSeconds)*time.Second, path, basePath)
	} else {
		// No explicit TTL: mark as owned by CacheCleaner so a
		// concurrent nocache request's short floor can't shorten
		// this file's lifetime.
		enqueueWithBase(services.GlobalExpiryQueue, services.OwnedThreshold, path, basePath)
	}
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

// ensureBase renders basePath if it's missing on disk. Concurrent
// callers for the same basePath are deduplicated via baseSfg, so
// sibling requests (same viewport, different overlays) do not
// duplicate base rendering. The cache index is not consulted — a
// stale entry could falsely claim the base existed while it had
// been removed by CacheCleaner or an external actor, leading the
// subsequent read to fail.
//
// Singleflight key correctness: basePath == BasePath() ==
// hash(WithoutDrawables()), so two callers share a slot iff their
// base-rendering inputs (style/lat/lon/zoom/w/h/scale/bearing/pitch/
// format) are identical. Base generation reads none of Markers,
// Polygons, or Circles, so merging the renders is safe.
func (h *StaticMapHandler) ensureBase(ctx context.Context, staticMap models.StaticMap, basePath string) error {
	_, err, _ := h.baseSfg.Do(basePath, func() (any, error) {
		if _, err := os.Stat(basePath); err == nil {
			return nil, nil
		}
		return nil, h.generateBaseStaticMap(ctx, staticMap, basePath)
	})
	return err
}

func (h *StaticMapHandler) generateStaticMap(ctx context.Context, path, basePath string, staticMap models.StaticMap) error {
	ensureDir(filepath.Dir(path))

	if err := h.ensureBase(ctx, staticMap, basePath); err != nil {
		return err
	}

	hasDrawables := len(staticMap.Markers) > 0 || len(staticMap.Polygons) > 0 || len(staticMap.Circles) > 0

	// If no drawables, just use base
	if !hasDrawables {
		// Copy or link base to path
		if path != basePath {
			data, err := os.ReadFile(basePath)
			if err != nil {
				return err
			}
			return atomicWriteFile(path, data, 0644)
		}
		return nil
	}

	// Download markers if needed
	if err := h.downloadMarkers(ctx, staticMap); err != nil {
		return err
	}

	// Generate with drawables
	return utils.GenerateStaticMap(staticMap, basePath, path, h.sphericalMercator)
}

func (h *StaticMapHandler) generateBaseStaticMap(ctx context.Context, staticMap models.StaticMap, basePath string) error {
	extStyle := h.stylesController.GetExternalStyle(staticMap.Style) // nil for local styles

	if extStyle == nil {
		// Local style: fractional zoom → viewport render (native float zoom).
		// Integer zoom → tile stitching (cacheable via Cache/Tile).
		if isFractional(staticMap.Zoom) {
			return h.generateBaseStaticMapFromAPIFn(ctx, staticMap, basePath)
		}
		return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, basePath, extStyle)
	}

	// External styles: tile stitching (no viewport endpoint available).
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

func (h *StaticMapHandler) generateBaseStaticMapFromAPI(ctx context.Context, staticMap models.StaticMap, basePath string) error {
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

	encoded, err := h.renderer.RenderViewport(ctx, renderer.ViewportRequest{
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
	if err != nil {
		return fmt.Errorf("renderer viewport: %w", err)
	}

	ensureDir(filepath.Dir(basePath))
	if err := atomicWriteFile(basePath, encoded, 0o644); err != nil {
		return fmt.Errorf("write base path: %w", err)
	}
	return nil
}

func (h *StaticMapHandler) generateBaseStaticMapFromTiles(ctx context.Context, staticMap models.StaticMap, basePath string, extStyle *models.Style) error {
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
			return fmt.Errorf("failed to generate tile: %w", slot.err)
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

	// Combine tiles
	return utils.GenerateBaseStaticMap(staticMap, tilePaths, basePath, offsetX, offsetY, hasScale, redownload)
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

func (h *StaticMapHandler) generateResponse(w http.ResponseWriter, r *http.Request, staticMap models.StaticMap, path string) {
	if handlePregenerateResponse(w, r, path, staticMap) {
		return
	}
	serveFile(w, r, path)
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

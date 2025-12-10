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
	"github.com/lenisko/rampardos/internal/utils"
)

// StaticMapHandler handles static map requests
type StaticMapHandler struct {
	tileServerURL     string
	tileHandler       *TileHandler
	statsController   *services.StatsController
	stylesController  *services.StylesController
	sphericalMercator *utils.SphericalMercator
}

// NewStaticMapHandler creates a new static map handler
func NewStaticMapHandler(tileServerURL string, tileHandler *TileHandler, statsController *services.StatsController, stylesController *services.StylesController) *StaticMapHandler {
	return &StaticMapHandler{
		tileServerURL:     tileServerURL,
		tileHandler:       tileHandler,
		statsController:   statsController,
		stylesController:  stylesController,
		sphericalMercator: utils.NewSphericalMercator(),
	}
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

// GenerateStaticMap generates a static map (used by MultiStaticMapHandler)
func (h *StaticMapHandler) GenerateStaticMap(ctx context.Context, staticMap models.StaticMap) error {
	path := staticMap.Path()
	basePath := staticMap.BasePath()

	// Check if already exists
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	// Check if base exists
	baseExists := false
	if _, err := os.Stat(basePath); err == nil {
		baseExists = true
	}

	return h.generateStaticMap(ctx, path, basePath, baseExists, staticMap)
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

	// Check if cache should be skipped (for preview)
	skipCache := r.URL.Query().Get("nocache") == "true"

	// Check if cached (use cache index first, then filesystem)
	cached := false
	if !skipCache {
		if services.GlobalCacheIndex != nil && services.GlobalCacheIndex.HasStaticMap(path) {
			cached = true
		} else if _, err := os.Stat(path); err == nil {
			cached = true
			if services.GlobalCacheIndex != nil {
				services.GlobalCacheIndex.AddStaticMap(path)
			}
		}
	}

	if cached {
		duration := time.Since(startTime).Seconds()
		slog.Debug("Served static map (cached)", "path", path)
		h.statsController.StaticMapServed(false, path, staticMap.Style)
		services.GlobalMetrics.RecordRequest("staticmap", staticMap.Style, true, duration)
		h.generateResponse(w, r, staticMap, path)
		return
	}

	// Check if base exists (use cache index first)
	baseExists := false
	if services.GlobalCacheIndex != nil && services.GlobalCacheIndex.HasStaticMap(basePath) {
		baseExists = true
	} else if _, err := os.Stat(basePath); err == nil {
		baseExists = true
		if services.GlobalCacheIndex != nil {
			services.GlobalCacheIndex.AddStaticMap(basePath)
		}
	}

	// Generate
	if err := h.generateStaticMap(r.Context(), path, basePath, baseExists, staticMap); err != nil {
		slog.Error("Failed to generate static map", "error", err)
		services.GlobalMetrics.RecordError("staticmap", "generation_failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Add to cache index
	if services.GlobalCacheIndex != nil {
		services.GlobalCacheIndex.AddStaticMap(path)
	}

	duration := time.Since(startTime).Seconds()
	slog.Debug("Served static map (generated)", "path", path, "duration", duration)
	h.statsController.StaticMapServed(true, path, staticMap.Style)
	services.GlobalMetrics.RecordRequest("staticmap", staticMap.Style, false, duration)
	h.generateResponse(w, r, staticMap, path)
}

func (h *StaticMapHandler) handlePregeneratedRequest(w http.ResponseWriter, r *http.Request, id string) {
	path := fmt.Sprintf("Cache/Static/%s", id)
	regeneratablePath := fmt.Sprintf("Cache/Regeneratable/%s.json", filepath.Base(path))

	// Check if exists
	if _, err := os.Stat(path); err == nil {
		slog.Debug("Served static map (pregenerated)")
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

func (h *StaticMapHandler) generateStaticMap(ctx context.Context, path, basePath string, baseExists bool, staticMap models.StaticMap) error {
	// Ensure cache directory exists (cached check)
	ensureDir(filepath.Dir(path))

	hasDrawables := len(staticMap.Markers) > 0 || len(staticMap.Polygons) > 0 || len(staticMap.Circles) > 0

	// Generate base if needed
	if !baseExists {
		if err := h.generateBaseStaticMap(ctx, staticMap, basePath); err != nil {
			return err
		}
	}

	// If no drawables, just use base
	if !hasDrawables {
		// Copy or link base to path
		if path != basePath {
			data, err := os.ReadFile(basePath)
			if err != nil {
				return err
			}
			return os.WriteFile(path, data, 0644)
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
	// Check if this is an external style (needs tile stitching)
	if extStyle := h.stylesController.GetExternalStyle(staticMap.Style); extStyle != nil {
		// External style - use tile stitching
		return h.generateBaseStaticMapFromTiles(ctx, staticMap, basePath, extStyle)
	}

	// Local style - use tileserver-gl's static API directly
	return h.generateBaseStaticMapFromAPI(ctx, staticMap, basePath)
}

func (h *StaticMapHandler) generateBaseStaticMapFromAPI(ctx context.Context, staticMap models.StaticMap, basePath string) error {
	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	scaleString := ""
	if scale > 1 {
		scaleString = fmt.Sprintf("@%dx", scale)
	}

	bearing := 0.0
	if staticMap.Bearing != nil {
		bearing = *staticMap.Bearing
	}
	pitch := 0.0
	if staticMap.Pitch != nil {
		pitch = *staticMap.Pitch
	}

	// Use tileserver-gl's static API: /styles/{style}/static/{lon},{lat},{zoom}@{bearing},{pitch}/{width}x{height}{scale}.{format}
	staticURL := fmt.Sprintf("%s/styles/%s/static/%f,%f,%f@%f,%f/%dx%d%s.%s",
		h.tileServerURL,
		staticMap.Style,
		staticMap.Longitude,
		staticMap.Latitude,
		staticMap.Zoom,
		bearing,
		pitch,
		staticMap.Width,
		staticMap.Height,
		scaleString,
		staticMap.GetFormat(),
	)

	slog.Debug("Fetching static map from tileserver", "url", staticURL)

	if err := services.DownloadFile(ctx, staticURL, basePath, "image", 0); err != nil {
		return fmt.Errorf("failed to load base static map: %w", err)
	}

	return nil
}

func (h *StaticMapHandler) generateBaseStaticMapFromTiles(ctx context.Context, staticMap models.StaticMap, basePath string, extStyle *models.Style) error {
	// Calculate tiles needed
	center := models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude}
	zoom := int(staticMap.Zoom)

	// Get center tile
	centerX, centerY, xDelta, yDelta := h.sphericalMercator.XY(center, zoom)

	// Calculate how many tiles we need
	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	tilesX := int(math.Ceil(float64(staticMap.Width)/256/float64(scale))) + 1
	tilesY := int(math.Ceil(float64(staticMap.Height)/256/float64(scale))) + 1

	// Check if external style supports scale
	hasScale := strings.Contains(extStyle.URL, "{@scale}") || strings.Contains(extStyle.URL, "{scale}")

	// Generate tiles
	var tilePaths []string
	for dy := -tilesY / 2; dy <= tilesY/2; dy++ {
		for dx := -tilesX / 2; dx <= tilesX/2; dx++ {
			tileX := centerX + dx
			tileY := centerY + dy

			result, err := h.tileHandler.GenerateTile(ctx, staticMap.Style, zoom, tileX, tileY, scale, staticMap.GetFormat())
			if err != nil {
				return fmt.Errorf("failed to generate tile: %w", err)
			}
			tilePaths = append(tilePaths, result.Path)
		}
	}

	// Calculate offset
	offsetX := int(xDelta) + (tilesX/2)*256
	offsetY := int(yDelta) + (tilesY/2)*256

	// Combine tiles
	return utils.GenerateBaseStaticMap(staticMap, tilePaths, basePath, offsetX, offsetY, hasScale)
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

		// Check cache index first (fast path)
		if services.GlobalCacheIndex != nil && services.GlobalCacheIndex.HasMarker(path) {
			h.statsController.MarkerServed(false, path, domain)
			continue
		}

		// Check filesystem
		if _, err := os.Stat(path); err == nil {
			// Add to cache index
			if services.GlobalCacheIndex != nil {
				services.GlobalCacheIndex.AddMarker(path)
			}
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
				// Try fallback
				if md.marker.FallbackURL != "" {
					fallbackHash := utils.PersistentHashString(md.marker.FallbackURL)
					fallbackPath := fmt.Sprintf("Cache/Marker/%s.%s", fallbackHash, md.format)
					fallbackDomain := extractDomain(md.marker.FallbackURL)

					// Check if fallback exists
					if services.GlobalCacheIndex != nil && services.GlobalCacheIndex.HasMarker(fallbackPath) {
						h.statsController.MarkerServed(false, fallbackPath, fallbackDomain)
						return
					}
					if _, err := os.Stat(fallbackPath); err == nil {
						if services.GlobalCacheIndex != nil {
							services.GlobalCacheIndex.AddMarker(fallbackPath)
						}
						h.statsController.MarkerServed(false, fallbackPath, fallbackDomain)
						return
					}

					if err := services.DownloadFile(ctx, md.marker.FallbackURL, fallbackPath, "", 0); err == nil {
						if services.GlobalCacheIndex != nil {
							services.GlobalCacheIndex.AddMarker(fallbackPath)
						}
						h.statsController.MarkerServed(true, fallbackPath, fallbackDomain)
					}
				}
			} else {
				if services.GlobalCacheIndex != nil {
					services.GlobalCacheIndex.AddMarker(md.path)
				}
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

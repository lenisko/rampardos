package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/utils"
)

// MultiStaticMapHandler handles multi static map requests
type MultiStaticMapHandler struct {
	staticMapHandler *StaticMapHandler
	statsController  *services.StatsController
	sfg              singleflight.Group
}

// NewMultiStaticMapHandler creates a new multi static map handler
func NewMultiStaticMapHandler(staticMapHandler *StaticMapHandler, statsController *services.StatsController) *MultiStaticMapHandler {
	return &MultiStaticMapHandler{
		staticMapHandler: staticMapHandler,
		statsController:  statsController,
	}
}

// Get handles GET /multistaticmap
func (h *MultiStaticMapHandler) Get(w http.ResponseWriter, r *http.Request) {
	var multiStaticMap models.MultiStaticMap
	if err := parseMultiQueryParams(r, &multiStaticMap); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, multiStaticMap)
}

// Post handles POST /multistaticmap
func (h *MultiStaticMapHandler) Post(w http.ResponseWriter, r *http.Request) {
	var multiStaticMap models.MultiStaticMap
	if err := json.NewDecoder(r.Body).Decode(&multiStaticMap); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, multiStaticMap)
}

// GetTemplate handles GET /multistaticmap/:template
func (h *MultiStaticMapHandler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	template := chi.URLParam(r, "template")
	if template == "" {
		http.Error(w, "Missing template", http.StatusBadRequest)
		return
	}
	services.GlobalMetrics.RecordTemplateRender(template, "get", "multistaticmap")

	multiStaticMap, err := h.renderTemplate(r, template, r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, *multiStaticMap)
}

// PostTemplate handles POST /multistaticmap/:template
func (h *MultiStaticMapHandler) PostTemplate(w http.ResponseWriter, r *http.Request) {
	template := chi.URLParam(r, "template")
	if template == "" {
		http.Error(w, "Missing template", http.StatusBadRequest)
		return
	}
	services.GlobalMetrics.RecordTemplateRender(template, "post", "multistaticmap")

	var context map[string]any
	if err := json.NewDecoder(r.Body).Decode(&context); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	multiStaticMap, err := h.renderTemplateWithContext(template, context)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, *multiStaticMap)
}

// GetPregenerated handles GET /multistaticmap/pregenerated/:id
func (h *MultiStaticMapHandler) GetPregenerated(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" || strings.Contains(id, "..") {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	h.handlePregeneratedRequest(w, r, id)
}

// HandleRequest handles a multi static map request (exported for templates controller)
func (h *MultiStaticMapHandler) HandleRequest(w http.ResponseWriter, r *http.Request, multiStaticMap models.MultiStaticMap) {
	h.handleRequest(w, r, multiStaticMap)
}

func (h *MultiStaticMapHandler) handleRequest(w http.ResponseWriter, r *http.Request, multiStaticMap models.MultiStaticMap) {
	path := multiStaticMap.Path()
	startTime := time.Now()

	services.GlobalMetrics.IncrementInFlight("multistaticmap")
	defer services.GlobalMetrics.DecrementInFlight("multistaticmap")

	skipCache := r.URL.Query().Get("nocache") == "true"
	pregenerate := r.URL.Query().Get("pregenerate") == "true"
	ttlStr := r.URL.Query().Get("ttl")
	var ttlSeconds int
	if ttlStr != "" {
		ttlSeconds, _ = strconv.Atoi(ttlStr)
	}
	if skipCache && pregenerate {
		skipCache = false
		if ttlSeconds == 0 {
			ttlSeconds = 30
		}
	}

	// Check if cached (use cache index first, then filesystem)
	cached := false
	if !skipCache {
		if services.GlobalCacheIndex != nil && services.GlobalCacheIndex.HasMultiStaticMap(path) {
			cached = true
		} else if _, err := os.Stat(path); err == nil {
			cached = true
			if services.GlobalCacheIndex != nil {
				services.GlobalCacheIndex.AddMultiStaticMap(path)
			}
		}
	}

	if cached {
		duration := time.Since(startTime).Seconds()
		slog.Debug("Served multi-static map (cached)", "file", filepath.Base(path))
		h.statsController.StaticMapServed(false, path, "multi")
		services.GlobalMetrics.RecordRequest("multistaticmap", "multi", true, duration)
		h.generateResponse(w, r, multiStaticMap, path)
		return
	}

	// Flatten component order for the stitcher.
	var components []models.StaticMap
	for _, grid := range multiStaticMap.Grid {
		for _, m := range grid.Maps {
			components = append(components, m.Map)
		}
	}
	mapCount := len(components)

	// Flow through nocache/ttl to component map generation.
	componentOpts := GenerateOpts{NoCache: skipCache}
	if ttlSeconds > 0 {
		componentOpts.TTL = time.Duration(ttlSeconds) * time.Second
	}

	// singleflight: deduplicate the entire generate+combine operation
	// for identical multistaticmap requests. Returns the final stitched
	// bytes so the caller can serve from memory.
	v, sfErr, _ := h.sfg.Do(path, func() (any, error) {
		if !skipCache {
			if b, err := os.ReadFile(path); err == nil {
				return b, nil
			}
		}

		// Parallel component generation, preserving slot order for the stitcher.
		componentBytes := make([][]byte, len(components))
		errCh := make(chan error, len(components))
		sem := make(chan struct{}, 5)
		var wg sync.WaitGroup
		for i, sm := range components {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, sm models.StaticMap) {
				defer wg.Done()
				defer func() { <-sem }()
				b, err := h.staticMapHandler.GenerateStaticMap(r.Context(), sm, componentOpts)
				if err != nil {
					errCh <- err
					return
				}
				componentBytes[i] = b
			}(i, sm)
		}
		wg.Wait()
		close(errCh)
		if err, ok := <-errCh; ok {
			return nil, err
		}

		stitched, err := utils.ComposeMultiStaticMapBytes(multiStaticMap, componentBytes)
		if err != nil {
			return nil, err
		}
		if !skipCache {
			if err := utils.SaveImageBytes(path, stitched); err != nil {
				return nil, err
			}
		}
		return stitched, nil
	})

	if sfErr != nil {
		slog.Error("Failed to generate multi-static map", "error", sfErr)
		services.GlobalMetrics.RecordError("multistaticmap", "generation_failed")
		http.Error(w, sfErr.Error(), http.StatusInternalServerError)
		return
	}

	var stitched []byte
	if v != nil {
		stitched, _ = v.([]byte)
	}

	duration := time.Since(startTime).Seconds()
	h.statsController.StaticMapServed(true, path, "multi")
	services.GlobalMetrics.RecordRequest("multistaticmap", "multi", false, duration)

	if skipCache {
		// nocache: serve bytes directly, no disk footprint.
		slog.Debug("Served multi-static map (nocache)", "file", filepath.Base(path), "maps", mapCount, "duration", duration)
		h.generateResponseBytes(w, r, multiStaticMap, path, stitched)
		return
	}

	if services.GlobalCacheIndex != nil {
		services.GlobalCacheIndex.AddMultiStaticMap(path)
	}
	slog.Debug("Served multi-static map (generated)", "file", filepath.Base(path), "maps", mapCount, "duration", duration, "ttl", ttlSeconds)
	h.generateResponseBytes(w, r, multiStaticMap, path, stitched)

	if ttlSeconds > 0 && services.GlobalExpiryQueue != nil {
		cleanupIndex := func() {
			if services.GlobalCacheIndex != nil {
				services.GlobalCacheIndex.RemoveMultiStaticMap(path)
			}
		}
		services.GlobalExpiryQueue.Add(time.Duration(ttlSeconds)*time.Second, cleanupIndex, path)
	}
}

func (h *MultiStaticMapHandler) handlePregeneratedRequest(w http.ResponseWriter, r *http.Request, id string) {
	path := fmt.Sprintf("Cache/StaticMulti/%s", id)
	regeneratablePath := fmt.Sprintf("Cache/Regeneratable/%s.json", filepath.Base(path))

	// Check if exists
	if _, err := os.Stat(path); err == nil {
		slog.Debug("Served multi-static map (pregenerated)")
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

	var multiStaticMap models.MultiStaticMap
	if err := json.Unmarshal(data, &multiStaticMap); err != nil {
		http.Error(w, "Failed to parse regeneratable", http.StatusInternalServerError)
		return
	}

	h.handleRequest(w, r, multiStaticMap)
}

func (h *MultiStaticMapHandler) generateResponse(w http.ResponseWriter, r *http.Request, multiStaticMap models.MultiStaticMap, path string) {
	if handlePregenerateResponse(w, r, path, multiStaticMap) {
		return
	}
	serveFile(w, r, path)
}

// generateResponseBytes prefers serving the stitched bytes in memory
// so the response is immune to external deletion of path between the
// atomic-write and serve. Falls back to serveFile if stitched is nil.
func (h *MultiStaticMapHandler) generateResponseBytes(w http.ResponseWriter, r *http.Request, msm models.MultiStaticMap, path string, stitched []byte) {
	if handlePregenerateResponse(w, r, path, msm) {
		return
	}
	if stitched != nil {
		w.Header().Set("Content-Type", models.ImageFormatPNG.ContentType())
		w.Header().Set("Content-Length", strconv.Itoa(len(stitched)))
		_, _ = w.Write(stitched)
		return
	}
	serveFile(w, r, path)
}

func (h *MultiStaticMapHandler) renderTemplate(_ *http.Request, template string, params map[string][]string) (*models.MultiStaticMap, error) {
	context := make(map[string]any)
	for k, v := range params {
		if len(v) == 1 {
			context[k] = v[0]
		} else {
			context[k] = v
		}
	}
	return h.renderTemplateWithContext(template, context)
}

func (h *MultiStaticMapHandler) renderTemplateWithContext(templateName string, context map[string]any) (*models.MultiStaticMap, error) {
	var multiStaticMap models.MultiStaticMap
	if err := renderTemplateToStruct(templateName, context, &multiStaticMap, "multistaticmap_template"); err != nil {
		return nil, err
	}
	return &multiStaticMap, nil
}

func parseMultiQueryParams(r *http.Request, multiStaticMap *models.MultiStaticMap) error {
	gridJSON := r.URL.Query().Get("grid")
	if gridJSON == "" {
		return fmt.Errorf("missing grid parameter")
	}

	if err := json.Unmarshal([]byte(gridJSON), &multiStaticMap.Grid); err != nil {
		return fmt.Errorf("invalid grid JSON: %w", err)
	}

	return nil
}

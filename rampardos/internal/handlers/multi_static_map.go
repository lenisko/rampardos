package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
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

	// Collect all maps to generate
	var mapsToGenerate []models.StaticMap
	for _, grid := range multiStaticMap.Grid {
		for _, m := range grid.Maps {
			mapsToGenerate = append(mapsToGenerate, m.Map)
		}
	}
	mapCount := len(mapsToGenerate)

	// Flow TTL to component generation. nocache collapses to the
	// 30-second TTL floor for any pregenerate-side persistence;
	// explicit ttl wins when larger.
	componentOpts := GenerateOpts{}
	if ttlSeconds > 0 {
		componentOpts.TTL = time.Duration(ttlSeconds) * time.Second
	} else if skipCache {
		componentOpts.TTL = nocacheBaseTTLFloor
	}

	// singleflight: deduplicate the entire generate+combine operation
	// for identical multistaticmap requests. Detach the caller's
	// cancellation so a leader disconnect does not abort the shared
	// composite render for concurrent followers.
	genCtx := context.WithoutCancel(r.Context())
	v, sfErr, _ := h.sfg.Do(path, func() (any, error) {
		// skipCache (from ?nocache=true) governs disk lifetime in the
		// pregenerate path, not in-memory reuse — concurrent multi
		// requests always share the LRU.
		if services.GlobalCompositeImageCache != nil {
			if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
				return cached, nil
			}
		}

		componentImages := make([]image.Image, len(mapsToGenerate))
		var wg sync.WaitGroup
		var genErr error
		var errOnce sync.Once
		sem := make(chan struct{}, 5)

		for i, staticMap := range mapsToGenerate {
			wg.Add(1)
			go func(idx int, sm models.StaticMap) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				img, err := h.staticMapHandler.GenerateStaticMap(genCtx, sm, componentOpts)
				if err != nil {
					errOnce.Do(func() { genErr = err })
					return
				}
				componentImages[idx] = img
			}(i, staticMap)
		}
		wg.Wait()

		if genErr != nil {
			return nil, genErr
		}

		composed, err := utils.GenerateMultiStaticMapFromImages(multiStaticMap, componentImages)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(path, composed)
		}
		return composed, nil
	})

	if sfErr != nil {
		slog.Error("Failed to generate multi-static map", "error", sfErr)
		services.GlobalMetrics.RecordError("multistaticmap", "generation_failed")
		http.Error(w, sfErr.Error(), http.StatusInternalServerError)
		return
	}
	composed := v.(image.Image)
	encoded, err := utils.EncodeImage(composed, filepath.Ext(path))
	if err != nil {
		slog.Error("Failed to encode multi-static map", "error", err)
		services.GlobalMetrics.RecordError("multistaticmap", "encode_failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	duration := time.Since(startTime).Seconds()
	h.statsController.StaticMapServed(true, path, "multi")
	services.GlobalMetrics.RecordRequest("multistaticmap", "multi", false, duration)

	ttl := effectiveTTL(ttlSeconds)
	slog.Debug("Served multi-static map", "file", filepath.Base(path), "maps", mapCount, "duration", duration, "ttl", ttlSeconds, "nocache", skipCache)
	h.generateResponse(w, r, multiStaticMap, path, encoded, ttl, path)
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

func (h *MultiStaticMapHandler) generateResponse(w http.ResponseWriter, r *http.Request, multiStaticMap models.MultiStaticMap, path string, encoded []byte, ttl time.Duration, basePath string) {
	if handlePregenerateResponseBytes(w, r, path, multiStaticMap, encoded, ttl, basePath) {
		return
	}
	serveStaticMapBytes(w, r, path, encoded)
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

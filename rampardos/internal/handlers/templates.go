package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/utils"
)

// TemplatesHandler handles template-related requests
type TemplatesHandler struct {
	templatesController   *services.TemplatesController
	staticMapHandler      *StaticMapHandler
	multiStaticMapHandler *MultiStaticMapHandler
}

// NewTemplatesHandler creates a new templates handler
func NewTemplatesHandler(templatesController *services.TemplatesController, staticMapHandler *StaticMapHandler, multiStaticMapHandler *MultiStaticMapHandler) *TemplatesHandler {
	return &TemplatesHandler{
		templatesController:   templatesController,
		staticMapHandler:      staticMapHandler,
		multiStaticMapHandler: multiStaticMapHandler,
	}
}

// PreviewRequest represents a template preview request
type PreviewRequest struct {
	Template string         `json:"template"`
	Context  map[string]any `json:"context"`
	Mode     string         `json:"mode"` // "StaticMap" or "MultiStaticMap"
}

// SaveRequest represents a template save request
type SaveRequest struct {
	Template string `json:"template"`
	Name     string `json:"name"`
	OldName  string `json:"oldName"`
}

// SaveTestDataRequest represents a test data save request
type SaveTestDataRequest struct {
	Name     string `json:"name"`
	TestData string `json:"testData"`
}

// Preview handles POST /admin/api/templates/preview
func (h *TemplatesHandler) Preview(w http.ResponseWriter, r *http.Request) {
	var req PreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		services.GlobalMetrics.RecordError("template_preview", "invalid_json")
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Render template with Jet
	content, err := services.GlobalJetRenderer.RenderString(req.Template, req.Context)
	if err != nil {
		slog.Error("Failed to render template", "error", err)
		services.GlobalMetrics.RecordError("template_preview", "render_failed")
		http.Error(w, fmt.Sprintf("Template rendering error: %s", err.Error()), http.StatusBadRequest)
		return
	}

	// Clean up trailing commas in JSON (common issue with template loops)
	content = utils.CleanJSONTrailingCommas(content)

	// Compact JSON for debug output
	if services.GlobalRuntimeSettings != nil && services.GlobalRuntimeSettings.IsDebugEnabled() {
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, []byte(content)); err == nil {
			fmt.Printf("[DEBUG] Rendered template: %s\n", compacted.String())
		}
	}

	// Add nocache parameter to skip cache for preview
	q := r.URL.Query()
	q.Set("nocache", "true")
	r.URL.RawQuery = q.Encode()

	switch req.Mode {
	case "StaticMap":
		var staticMap models.StaticMap
		if err := json.NewDecoder(bytes.NewReader([]byte(content))).Decode(&staticMap); err != nil {
			services.GlobalMetrics.RecordError("template_preview", "decode_staticmap_failed")
			http.Error(w, fmt.Sprintf("Invalid Template: %s", err.Error()), http.StatusBadRequest)
			return
		}
		h.staticMapHandler.handleRequest(w, r, staticMap)
	case "MultiStaticMap":
		var multiStaticMap models.MultiStaticMap
		if err := json.NewDecoder(bytes.NewReader([]byte(content))).Decode(&multiStaticMap); err != nil {
			slog.Error("Failed to decode MultiStaticMap", "error", err, "content_preview", content[:min(500, len(content))])
			services.GlobalMetrics.RecordError("template_preview", "decode_multistaticmap_failed")
			http.Error(w, fmt.Sprintf("Invalid Template: %s", err.Error()), http.StatusBadRequest)
			return
		}
		slog.Debug("Decoded MultiStaticMap", "grid_count", len(multiStaticMap.Grid))
		h.multiStaticMapHandler.HandleRequest(w, r, multiStaticMap)
	default:
		services.GlobalMetrics.RecordError("template_preview", "invalid_mode")
		http.Error(w, "Invalid mode", http.StatusBadRequest)
	}
}

// Save handles POST /admin/api/templates/save
func (h *TemplatesHandler) Save(w http.ResponseWriter, r *http.Request) {
	var req SaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.templatesController.SaveTemplate(req.Name, req.OldName, req.Template); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update Jet template cache
	if services.GlobalJetRenderer != nil {
		// If name changed, delete old template from cache
		if req.OldName != "" && req.OldName != req.Name {
			services.GlobalJetRenderer.DeleteTemplate(req.OldName)
		}
		services.GlobalJetRenderer.SetTemplate(req.Name, req.Template)
	}

	w.WriteHeader(http.StatusOK)
}

// Delete handles DELETE /admin/api/templates/delete/:name
func (h *TemplatesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "Name parameter is required", http.StatusBadRequest)
		return
	}

	if err := h.templatesController.DeleteTemplate(name); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Template not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove from Jet template cache
	if services.GlobalJetRenderer != nil {
		services.GlobalJetRenderer.DeleteTemplate(name)
	}

	w.WriteHeader(http.StatusOK)
}

// SaveTestData handles POST /admin/api/templates/testdata
func (h *TemplatesHandler) SaveTestData(w http.ResponseWriter, r *http.Request) {
	var req SaveTestDataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	if err := h.templatesController.SaveTestData(req.Name, req.TestData); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

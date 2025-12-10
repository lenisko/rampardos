package handlers

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
)

// StylesHandler handles style-related requests
type StylesHandler struct {
	stylesController *services.StylesController
}

// NewStylesHandler creates a new styles handler
func NewStylesHandler(stylesController *services.StylesController) *StylesHandler {
	return &StylesHandler{
		stylesController: stylesController,
	}
}

// Get handles GET /styles
func (h *StylesHandler) Get(w http.ResponseWriter, r *http.Request) {
	styles, err := h.stylesController.GetStyles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(styles)
}

// AddExternal handles POST /admin/api/styles/external/add
func (h *StylesHandler) AddExternal(w http.ResponseWriter, r *http.Request) {
	var style models.Style
	if err := json.NewDecoder(r.Body).Decode(&style); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.stylesController.AddExternalStyle(style); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteExternal handles DELETE /admin/api/styles/external/:id
func (h *StylesHandler) DeleteExternal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "ID parameter is required", http.StatusBadRequest)
		return
	}

	if err := h.stylesController.DeleteExternalStyle(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// AddLocal handles POST /admin/api/styles/local/add
func (h *StylesHandler) AddLocal(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form (max 50MB)
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	id := r.FormValue("id")
	name := r.FormValue("name")
	if id == "" || name == "" {
		http.Error(w, "ID and name are required", http.StatusBadRequest)
		return
	}

	// Get ZIP file
	file, _, err := r.FormFile("zip")
	if err != nil {
		http.Error(w, "ZIP file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read ZIP data
	zipData, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Failed to read ZIP file", http.StatusInternalServerError)
		return
	}

	// Add local style
	if err := h.stylesController.AddLocalStyle(id, name, zipData); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Redirect to styles page
	http.Redirect(w, r, "/admin/styles", http.StatusSeeOther)
}

// DeleteLocal handles DELETE /admin/api/styles/local/:id
func (h *StylesHandler) DeleteLocal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "ID parameter is required", http.StatusBadRequest)
		return
	}

	if err := h.stylesController.DeleteLocalStyle(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

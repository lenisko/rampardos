package handlers

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/services"
)

// FontsHandler handles font-related requests
type FontsHandler struct {
	fontsController *services.FontsController
}

// NewFontsHandler creates a new fonts handler
func NewFontsHandler(fontsController *services.FontsController) *FontsHandler {
	return &FontsHandler{
		fontsController: fontsController,
	}
}

// Add handles POST /admin/api/fonts/add
func (h *FontsHandler) Add(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form
	if err := r.ParseMultipartForm(64 << 20); err != nil { // 64MB max
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}

	if err := h.fontsController.AddFont(data, header.Filename); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/admin/fonts", http.StatusSeeOther)
}

// Delete handles DELETE /admin/api/fonts/delete/:name
func (h *FontsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "Name parameter is required", http.StatusBadRequest)
		return
	}

	if err := h.fontsController.DeleteFont(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetFile handles GET /admin/api/fonts/file/{name} - serves the original font file
func (h *FontsHandler) GetFile(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "Name parameter is required", http.StatusBadRequest)
		return
	}

	fontPath, err := h.fontsController.GetFontFile(name)
	if err != nil {
		http.Error(w, "Font file not found", http.StatusNotFound)
		return
	}

	http.ServeFile(w, r, fontPath)
}

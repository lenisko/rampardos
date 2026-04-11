package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/services"
)

// SettingsHandler handles settings-related requests
type SettingsHandler struct{}

// NewSettingsHandler creates a new settings handler
func NewSettingsHandler() *SettingsHandler {
	return &SettingsHandler{}
}

// DebugStatusResponse is the response for debug status
type DebugStatusResponse struct {
	Enabled bool `json:"enabled"`
}

// GetDebugStatus handles GET /admin/api/settings/debug
func (h *SettingsHandler) GetDebugStatus(w http.ResponseWriter, r *http.Request) {
	enabled := false
	if services.GlobalRuntimeSettings != nil {
		enabled = services.GlobalRuntimeSettings.IsDebugEnabled()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DebugStatusResponse{Enabled: enabled})
}

// ToggleDebug handles POST /admin/api/settings/debug/toggle
func (h *SettingsHandler) ToggleDebug(w http.ResponseWriter, r *http.Request) {
	if services.GlobalRuntimeSettings == nil {
		http.Error(w, "Runtime settings not initialized", http.StatusInternalServerError)
		return
	}

	newState := services.GlobalRuntimeSettings.ToggleDebug()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DebugStatusResponse{Enabled: newState})
}

// CacheDropResponse is the response for cache drop
type CacheDropResponse struct {
	Scheduled bool   `json:"scheduled"`
	Delay     uint32 `json:"delay"`
}

// DropCache handles POST /admin/api/cache/drop/{folder}
func (h *SettingsHandler) DropCache(w http.ResponseWriter, r *http.Request) {
	folder := chi.URLParam(r, "folder")
	if folder == "" {
		http.Error(w, "Folder parameter is required", http.StatusBadRequest)
		return
	}

	cleaner := services.GetCacheCleaner(folder)
	if cleaner == nil {
		http.Error(w, "Cache cleaner not found for folder: "+folder, http.StatusNotFound)
		return
	}

	cleaner.ScheduleDropAll()
	delay := services.GetCacheCleanerDelay(folder)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CacheDropResponse{Scheduled: true, Delay: delay})
}

// GetCacheDelay handles GET /admin/api/cache/delay/{folder}
func (h *SettingsHandler) GetCacheDelay(w http.ResponseWriter, r *http.Request) {
	folder := chi.URLParam(r, "folder")
	if folder == "" {
		http.Error(w, "Folder parameter is required", http.StatusBadRequest)
		return
	}

	delay := services.GetCacheCleanerDelay(folder)
	if delay == 0 {
		http.Error(w, "Cache cleaner not found for folder: "+folder, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CacheDropResponse{Delay: delay})
}

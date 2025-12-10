package handlers

import (
	"encoding/json"
	"net/http"

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

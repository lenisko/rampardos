package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/lenisko/rampardos/internal/utils"
)

// ConvertHandler handles template conversion requests
type ConvertHandler struct{}

// NewConvertHandler creates a new convert handler
func NewConvertHandler() *ConvertHandler {
	return &ConvertHandler{}
}

// ConvertRequest represents a conversion request
type ConvertRequest struct {
	Template string `json:"template"`
}

// ConvertResponse represents a conversion response
type ConvertResponse struct {
	Result string `json:"result"`
}

// LeafToJet handles POST /admin/api/convert/leaf-to-jet
func (h *ConvertHandler) LeafToJet(w http.ResponseWriter, r *http.Request) {
	var req ConvertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	converter := utils.NewLeafToJetConverter()
	result := converter.Convert(req.Template)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ConvertResponse{Result: result})
}

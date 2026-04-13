package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/services/renderer"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// Only allow same-origin requests for WebSocket connections
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // Allow requests without Origin header (same-origin)
		}
		// Compare origin with the request host
		host := r.Host
		return strings.Contains(origin, host)
	},
}

// DatasetsHandler handles dataset-related requests
type DatasetsHandler struct {
	datasetsController *services.DatasetsController
	downloadManager    *services.DownloadManager
	renderer           renderer.Renderer
}

// NewDatasetsHandler creates a new datasets handler
func NewDatasetsHandler(datasetsController *services.DatasetsController, r renderer.Renderer) *DatasetsHandler {
	h := &DatasetsHandler{
		datasetsController: datasetsController,
		renderer:           r,
	}

	// Create download manager with completion callback
	h.downloadManager = services.NewDownloadManager(func(name string, err error) {
		if err != nil {
			slog.Error("Download failed", "name", name, "error", err)
			return
		}

		slog.Info("Download complete, marking as uncombined", "name", name)
		h.datasetsController.MarkUncombined(name)
		h.datasetsController.UpdateDatasetSize(name)
	})

	return h
}

// Download handles WebSocket /admin/api/datasets/add (download from URL)
func (h *DatasetsHandler) Download(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Error("WebSocket read error", "error", err)
			}
			break
		}

		text := string(message)
		parts := strings.SplitN(text, ";", 2)
		if len(parts) != 2 {
			conn.WriteMessage(websocket.TextMessage, []byte("error:Invalid URL"))
			continue
		}

		name := parts[0]
		url := parts[1]

		// Sanitize name to prevent path traversal
		sanitized, err := services.SanitizeName(name)
		if err != nil {
			conn.WriteMessage(websocket.TextMessage, []byte("error:Invalid dataset name"))
			continue
		}

		path := filepath.Join(h.datasetsController.GetListFolder(), sanitized+".mbtiles")

		// Start background download
		if err := h.downloadManager.StartDownload(sanitized, url, path); err != nil {
			slog.Error("Failed to start download", "error", err)
			conn.WriteMessage(websocket.TextMessage, []byte("error:"+err.Error()))
			continue
		}

		slog.Info("Download started in background", "name", name)
		conn.WriteMessage(websocket.TextMessage, []byte("started"))
	}
}

// GetDownloadStatus handles GET /admin/api/datasets/downloads
func (h *DatasetsHandler) GetDownloadStatus(w http.ResponseWriter, r *http.Request) {
	downloads := h.downloadManager.GetAllDownloads()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(downloads)
}

// GetDownloadManager returns the download manager for use by views
func (h *DatasetsHandler) GetDownloadManager() *services.DownloadManager {
	return h.downloadManager
}

// ClearDownload handles POST /admin/api/datasets/downloads/{name}/clear
func (h *DatasetsHandler) ClearDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	h.downloadManager.ClearDownload(name)
	w.WriteHeader(http.StatusOK)
}

// CancelDownload handles POST /admin/api/datasets/downloads/{name}/cancel
func (h *DatasetsHandler) CancelDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.downloadManager.CancelDownload(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slog.Info("Download cancelled", "name", name)
	w.WriteHeader(http.StatusOK)
}

// CombineRequest represents the request body for combining datasets
type CombineRequest struct {
	Datasets []string `json:"datasets"`
}

// Combine handles POST /admin/api/datasets/combine
func (h *DatasetsHandler) Combine(w http.ResponseWriter, r *http.Request) {
	var req CombineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Datasets) < 2 {
		http.Error(w, "At least 2 datasets required", http.StatusBadRequest)
		return
	}

	slog.Info("Combining mbtiles", "datasets", req.Datasets)
	if err := h.datasetsController.CombineSelected(req.Datasets); err != nil {
		slog.Error("Combine failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.datasetsController.ClearUncombined()
	slog.Info("Combine complete")
	w.WriteHeader(http.StatusOK)
}

// SetActive handles POST /admin/api/datasets/{name}/activate
func (h *DatasetsHandler) SetActive(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	slog.Info("Setting active dataset", "name", name)
	if err := h.datasetsController.SetActive(name); err != nil {
		slog.Error("SetActive failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("Dataset activated", "name", name)
	w.WriteHeader(http.StatusOK)
}

// Delete handles WebSocket /admin/api/datasets/delete
func (h *DatasetsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Error("WebSocket read error", "error", err)
			}
			break
		}

		name := string(message)
		slog.Info("Deleting dataset", "name", name)

		if err := h.datasetsController.DeleteDataset(name); err != nil {
			slog.Error("Delete failed", "error", err)
			conn.WriteMessage(websocket.TextMessage, []byte(err.Error()))
			continue
		}

		// Clear from download manager to remove from UI
		h.downloadManager.ClearDownload(name)

		conn.WriteMessage(websocket.TextMessage, []byte("ok"))
	}
}

// ReloadTileserver handles POST /admin/api/datasets/reload-tileserver
func (h *DatasetsHandler) ReloadTileserver(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := h.renderer.ReloadStyles(ctx); err != nil {
		slog.Error("Renderer reload failed", "error", err)
		http.Error(w, "Failed to reload renderer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if cleaner := services.GetCacheCleaner("Tile"); cleaner != nil {
		cleaner.ScheduleDropAll()
	}
	slog.Info("Renderer reloaded")
	w.WriteHeader(http.StatusOK)
}

// Add handles POST /admin/api/datasets/add (file upload)
func (h *DatasetsHandler) Add(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form with large limit for mbtiles
	if err := r.ParseMultipartForm(128 << 30); err != nil { // 128GB max
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "Missing name", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("file")
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

	if err := h.datasetsController.AddDataset(name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

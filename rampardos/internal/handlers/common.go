package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// knownDirs caches directories that have been created to avoid repeated syscalls
var knownDirs sync.Map

// ensureDir creates a directory if it doesn't exist, using a cache to avoid repeated syscalls
func ensureDir(dir string) {
	if _, ok := knownDirs.Load(dir); ok {
		return
	}
	os.MkdirAll(dir, 0755)
	knownDirs.Store(dir, struct{}{})
}

// serveFile serves a file with cache headers
func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set("Cache-Control", "max-age=604800, must-revalidate")
	http.ServeFile(w, r, path)
}

// handlePregenerateResponse handles pregenerate query param and saves regeneratable data if needed.
// Returns true if pregenerate was handled (caller should return), false to continue with normal response.
func handlePregenerateResponse(w http.ResponseWriter, r *http.Request, path string, data any) bool {
	pregenerate := r.URL.Query().Get("pregenerate") == "true"
	if !pregenerate {
		return false
	}

	regeneratable := r.URL.Query().Get("regeneratable") == "true"
	if regeneratable {
		regeneratablePath := fmt.Sprintf("Cache/Regeneratable/%s.json", filepath.Base(path))
		if _, err := os.Stat(regeneratablePath); os.IsNotExist(err) {
			ensureDir(filepath.Dir(regeneratablePath))
			jsonData, _ := json.Marshal(data)
			os.WriteFile(regeneratablePath, jsonData, 0644)
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(filepath.Base(path)))
	return true
}

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/lenisko/rampardos/internal/services"
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

// atomicWriteFile writes data to path atomically with the requested
// file mode. Delegates to services.SaveBytesAtomic for the write +
// rename, then chmods the final path to perm.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := services.SaveBytesAtomic(path, data); err != nil {
		return err
	}
	return os.Chmod(path, perm)
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
			if jsonData, err := json.Marshal(data); err == nil {
				atomicWriteFile(regeneratablePath, jsonData, 0644)
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(filepath.Base(path)))
	return true
}

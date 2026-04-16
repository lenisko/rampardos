package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lenisko/rampardos/internal/services"
)

// nocacheBaseTTLFloor is the minimum TTL applied to a shared basePath
// when the owning request was nocache. Without this floor, a burst of
// concurrent requests at the same viewport (e.g. mass weather alerts)
// would each regenerate the base. The floor lets the render be reused
// across the burst without persisting for days. Tiles are already
// cached, so re-stitching after the floor expires is cheap.
const nocacheBaseTTLFloor = 30 * time.Second

// enqueueWithBase schedules path and, when distinct, basePath for
// deletion after ttl. For single-path callers (e.g. multi handler
// whose composite has no shared base), pass path == basePath.
func enqueueWithBase(q *services.ExpiryQueue, ttl time.Duration, path, basePath string) {
	if q == nil {
		return
	}
	if path != basePath {
		q.Add(ttl, path, basePath)
	} else {
		q.Add(ttl, path)
	}
}

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

// atomicWriteFile writes data to a temporary file then renames it to
// the target path. os.Rename is atomic on POSIX, so readers never see
// a partially-written file. This prevents corruption when multiple
// goroutines race to generate the same cached file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	ensureDir(filepath.Dir(path))
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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

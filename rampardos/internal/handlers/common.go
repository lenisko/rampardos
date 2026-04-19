package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lenisko/rampardos/internal/fileutil"
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

// serveFile serves a file with cache headers
func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set("Cache-Control", "max-age=604800, must-revalidate")
	http.ServeFile(w, r, path)
}

// handlePregenerateResponseBytes is the single disk-write site in
// the bytes-first pipeline. When pregenerate=true it writes the
// encoded image to `path` and enqueues the corresponding
// deletion/ownership in the expiry queue. Returns true if
// pregenerate was handled (caller should return).
//
// The enqueue lives here (not at the handler level) because
// writes-to-disk and expiry-registration must be one-to-one —
// enqueueing a path the caller never wrote was a footgun in the
// pre-bytes-first pipeline and no longer exists in this one.
func handlePregenerateResponseBytes(
	w http.ResponseWriter,
	r *http.Request,
	path string,
	data any,
	encoded []byte,
	ttl time.Duration,
	basePath string,
) bool {
	pregenerate := r.URL.Query().Get("pregenerate") == "true"
	if !pregenerate {
		return false
	}

	if err := fileutil.AtomicWriteFile(path, encoded, 0o644); err != nil {
		slog.Error("pregenerate write failed", "path", path, "error", err)
		http.Error(w, "pregenerate failed", http.StatusInternalServerError)
		return true
	}
	enqueueWithBase(services.GlobalExpiryQueue, ttl, path, basePath)

	regeneratable := r.URL.Query().Get("regeneratable") == "true"
	if regeneratable {
		regeneratablePath := fmt.Sprintf("Cache/Regeneratable/%s.json", filepath.Base(path))
		if _, err := os.Stat(regeneratablePath); os.IsNotExist(err) {
			if jsonData, err := json.Marshal(data); err == nil {
				fileutil.AtomicWriteFile(regeneratablePath, jsonData, 0644)
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(filepath.Base(path)))
	return true
}

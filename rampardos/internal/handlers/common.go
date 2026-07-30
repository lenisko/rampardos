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
	if os.MkdirAll(dir, 0755) == nil {
		knownDirs.Store(dir, struct{}{})
	}
}

// serveFile serves an on-disk file with the same content-addressable
// ETag + Cache-Control as the bytes-first response path.
// http.ServeFile dispatches to http.ServeContent internally, which
// honours the ETag we set here for conditional-GET 304 responses.
func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	setStaticMapCacheHeaders(w, path)
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
				// Best effort: the response is already decided, so a failed
				// marker must not fail the request — but it does mean this
				// entry will not be treated as regeneratable later, which is
				// worth knowing about.
				if err := fileutil.AtomicWriteFile(regeneratablePath, jsonData, 0644); err != nil {
					slog.Warn("Failed to write regeneratable marker", "path", regeneratablePath, "error", err)
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(filepath.Base(path)))
	return true
}

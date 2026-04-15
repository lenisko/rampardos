package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/utils"
)

// TestMarkerDeletedBetweenDownloadAndDrawDoesNotFail verifies that
// marker bytes held in memory survive external deletion of the
// Cache/Marker file. Before Task 4b, drawOverlays read markers from
// disk and would fail here.
func TestMarkerDeletedBetweenDownloadAndDrawDoesNotFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fakePNG(t))
	}))
	defer srv.Close()

	h, cleanup := newTestStaticMapHandler(t)
	defer cleanup()

	sm := models.StaticMap{
		Markers: []models.Marker{{URL: srv.URL + "/a.png", Latitude: 0, Longitude: 0}},
	}

	markers, err := h.downloadMarkerBytes(context.Background(), sm)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	key := utils.GetMarkerPath(sm.Markers[0])
	if len(markers[key]) == 0 {
		t.Fatalf("expected bytes at %s; got %v", key, markers)
	}

	// Simulate CacheCleaner deleting the on-disk marker mid-request.
	_ = os.Remove(key)

	// Render must still succeed because markers are in memory.
	baseBytes := fakePNG(t)
	merc := utils.NewSphericalMercator()
	sm.Width, sm.Height = 16, 16
	sm.Zoom = 10
	if _, err := utils.GenerateStaticMapBytes(sm, baseBytes, markers, merc); err != nil {
		t.Fatalf("render after marker deletion: %v", err)
	}
}

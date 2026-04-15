package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/utils"
)

// fakePNG returns a valid 1x1 PNG for tests.
func fakePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// newTestStaticMapHandler builds a handler with a tmp working dir and
// minimal dependencies for marker-download testing. Returns handler
// and cleanup func.
func newTestStaticMapHandler(t *testing.T) (*StaticMapHandler, func()) {
	t.Helper()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// DownloadBytes requires globalHTTPService to be initialised.
	if !services.HTTPServiceInitialized() {
		services.InitHTTPServiceForTest()
	}

	// Pass nil statsController — downloadMarkerBytes guards against nil.
	h := &StaticMapHandler{
		sphericalMercator: utils.NewSphericalMercator(),
		statsController:   nil,
	}

	return h, func() {
		_ = os.Chdir(origDir)
	}
}

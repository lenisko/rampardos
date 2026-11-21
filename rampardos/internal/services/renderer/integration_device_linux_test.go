//go:build renderer_integration

package renderer

import (
	"context"
	"testing"
	"time"

	"github.com/lenisko/rampardos/internal/models"
	"maplibre.org/maplibre-native-ffi/bindings/go/maplibre"
)

// newTestDevice creates the offscreen device NewGoPoolRenderer would, for
// tests that build a pool directly: without one each session would make
// its own GLES device, unlike production.
func newTestDevice(t *testing.T) *maplibre.GPUDeviceHandle {
	t.Helper()
	d, err := maplibre.NewGPUDevice(gpuDeviceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Release() })
	return d
}

// Every worker renders on the renderer's one offscreen device, so a
// retiring worker must release only its own session, never the device
// the other workers still render on. Scale 1 and 2 pools share it too.
func TestOffscreenDeviceSurvivesWorkerRetirement(t *testing.T) {
	cfg := setupIntegrationFixtures(t)
	cfg.PoolSize = 2
	cfg.StylePoolSize = 2
	ri, err := NewGoPoolRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ri.Close()
	r := ri.(*GoPoolRenderer)
	if r.device == nil {
		t.Fatal("renderer has no offscreen device")
	}

	render := func(scale uint8) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := r.Render(ctx, Request{StyleID: "bg", Z: 0, X: 0, Y: 0, Scale: scale, Format: models.ImageFormatPNG})
		if err != nil {
			t.Fatalf("scale %d: %v", scale, err)
		}
		if len(out) < 100 || string(out[1:4]) != "PNG" {
			t.Fatalf("scale %d: not a PNG (%d bytes)", scale, len(out))
		}
	}
	render(1)
	render(2)

	p, err := r.getOrCreatePool("bg", 1)
	if err != nil {
		t.Fatal(err)
	}
	p.grow()
	retired := false
	for range 3 {
		if retired = p.reapOnce(); retired {
			break
		}
	}
	if !retired {
		t.Fatal("pool did not retire its grown worker")
	}
	// Give the retiring worker time to tear its session down.
	time.Sleep(500 * time.Millisecond)

	render(1)
	render(2)
	if r.device == nil {
		t.Fatal("offscreen device gone after a worker retired")
	}
}

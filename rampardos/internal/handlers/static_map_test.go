package handlers

import (
	"context"
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services/renderer"
)

func TestIsFractional(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want bool
	}{
		{"zero", 0, false},
		{"integer positive", 14, false},
		{"integer negative", -3, false},
		{"large integer", 22, false},
		{"fractional", 14.7, true},
		{"tiny fractional", 14.001, true},
		{"just below integer", 13.9999, true},
		{"off by epsilon high", 14 + 1e-10, false},
		{"off by epsilon low", 14 - 1e-10, false},
		{"NaN", math.NaN(), false},
		{"positive inf", math.Inf(1), false},
		{"negative inf", math.Inf(-1), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isFractional(tc.in)
			if got != tc.want {
				t.Errorf("isFractional(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// stubDispatchStatic builds a handler whose generateBase* methods
// record which path was taken, so dispatch tests stay pure and never
// hit disk or network.
type dispatchRecord struct {
	fromAPI   int
	fromTiles int
	lastExt   *models.Style
	lastWarn  bool
}

func newDispatchHandlerForTest(t *testing.T, ext *models.Style, rec *dispatchRecord) *StaticMapHandler {
	t.Helper()
	h := &StaticMapHandler{
		stylesController: stubStylesController{ext: ext},
	}
	h.generateBaseStaticMapFromAPIFn = func(ctx context.Context, sm models.StaticMap) (image.Image, error) {
		rec.fromAPI++
		return image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil
	}
	h.generateBaseStaticMapFromTilesFn = func(ctx context.Context, sm models.StaticMap, basePath string, extStyle *models.Style) (image.Image, error) {
		rec.fromTiles++
		rec.lastExt = extStyle
		return image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil
	}
	h.logExternalViewportApproxFn = func(sm models.StaticMap) {
		rec.lastWarn = true
	}
	return h
}

type stubStylesController struct{ ext *models.Style }

func (s stubStylesController) GetExternalStyle(name string) *models.Style { return s.ext }

func TestGenerateBaseStaticMapDispatch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	basePath := filepath.Join(tmp, "base.png")
	_ = os.Remove(basePath)

	t.Run("local integer zoom -> stitch", func(t *testing.T) {
		rec := &dispatchRecord{}
		h := newDispatchHandlerForTest(t, nil, rec)
		sm := models.StaticMap{Style: "local", Zoom: 14, Width: 512, Height: 512}
		if _, err := h.generateBaseStaticMap(ctx, sm, basePath); err != nil {
			t.Fatal(err)
		}
		if rec.fromTiles != 1 || rec.fromAPI != 0 {
			t.Errorf("want stitch, got fromTiles=%d fromAPI=%d", rec.fromTiles, rec.fromAPI)
		}
		if rec.lastExt != nil {
			t.Errorf("want nil extStyle for local, got %+v", rec.lastExt)
		}
		if rec.lastWarn {
			t.Errorf("did not expect approximation warning")
		}
	})

	t.Run("external integer zoom -> stitch", func(t *testing.T) {
		rec := &dispatchRecord{}
		ext := &models.Style{ID: "ext", URL: "https://x/y/{z}/{x}/{y}.png"}
		h := newDispatchHandlerForTest(t, ext, rec)
		sm := models.StaticMap{Style: "ext", Zoom: 14, Width: 512, Height: 512}
		if _, err := h.generateBaseStaticMap(ctx, sm, basePath); err != nil {
			t.Fatal(err)
		}
		if rec.fromTiles != 1 || rec.fromAPI != 0 {
			t.Errorf("want stitch, got fromTiles=%d fromAPI=%d", rec.fromTiles, rec.fromAPI)
		}
		if rec.lastExt != ext {
			t.Errorf("want ext passed through, got %+v", rec.lastExt)
		}
		if rec.lastWarn {
			t.Errorf("did not expect approximation warning for integer zoom")
		}
	})

	t.Run("local fractional zoom -> viewport API", func(t *testing.T) {
		rec := &dispatchRecord{}
		h := newDispatchHandlerForTest(t, nil, rec)
		sm := models.StaticMap{Style: "local", Zoom: 14.7, Width: 512, Height: 512}
		if _, err := h.generateBaseStaticMap(ctx, sm, basePath); err != nil {
			t.Fatal(err)
		}
		if rec.fromAPI != 1 || rec.fromTiles != 0 {
			t.Errorf("want viewport API, got fromAPI=%d fromTiles=%d", rec.fromAPI, rec.fromTiles)
		}
		if rec.lastWarn {
			t.Errorf("local viewport does not need approximation warning")
		}
	})

	t.Run("external fractional zoom -> stitch with warning", func(t *testing.T) {
		rec := &dispatchRecord{}
		ext := &models.Style{ID: "ext", URL: "https://x/y/{z}/{x}/{y}.png"}
		h := newDispatchHandlerForTest(t, ext, rec)
		sm := models.StaticMap{Style: "ext", Zoom: 14.7, Width: 512, Height: 512}
		if _, err := h.generateBaseStaticMap(ctx, sm, basePath); err != nil {
			t.Fatal(err)
		}
		if rec.fromTiles != 1 || rec.fromAPI != 0 {
			t.Errorf("want stitch, got fromTiles=%d fromAPI=%d", rec.fromTiles, rec.fromAPI)
		}
		if !rec.lastWarn {
			t.Errorf("expected approximation warning for external fractional zoom")
		}
	})
}

func TestGenerateBaseStaticMapFromAPIUsesRenderer(t *testing.T) {
	fake := &renderer.Fake{}
	h := &StaticMapHandler{
		renderer:         fake,
		stylesController: stubStylesController{ext: nil},
	}
	h.generateBaseStaticMapFromAPIFn = h.generateBaseStaticMapFromAPI
	h.generateBaseStaticMapFromTilesFn = h.generateBaseStaticMapFromTiles
	h.logExternalViewportApproxFn = h.logExternalViewportApprox

	sm := models.StaticMap{
		Style:     "local",
		Zoom:      14.7,
		Latitude:  51.5074,
		Longitude: -0.1278,
		Width:     512,
		Height:    512,
		Scale:     1,
	}
	img, err := h.generateBaseStaticMapFromAPI(context.Background(), sm)
	if err != nil {
		t.Fatal(err)
	}
	if img == nil {
		t.Fatal("expected non-nil image")
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 renderer call, got %d", len(fake.Calls))
	}
	call := fake.Calls[0]
	if call.Kind != "RenderViewport" {
		t.Errorf("call kind: got %q, want RenderViewport", call.Kind)
	}
	if call.Viewport.Zoom != 14.7 {
		t.Errorf("zoom: got %v, want 14.7", call.Viewport.Zoom)
	}
}

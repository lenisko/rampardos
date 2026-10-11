package utils

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
)

// TestFitMarkerSize locks in ImageMagick `-resize WxH` geometry (what
// SwiftTileserverCache used): scale to fit inside the box, keeping the
// source aspect ratio, enlarging or shrinking as needed.
func TestFitMarkerSize(t *testing.T) {
	cases := []struct {
		name         string
		srcW, srcH   int
		boxW, boxH   int
		wantW, wantH int
	}{
		{"square into square", 128, 128, 60, 60, 60, 60},
		{"tall sprite (wwm 25)", 117, 128, 60, 60, 55, 60},
		{"wide sprite (wwm 320)", 128, 73, 60, 60, 60, 34},
		{"very wide sprite (wwm 382)", 128, 49, 60, 60, 60, 23},
		{"very wide sprite scale=2", 128, 49, 120, 120, 120, 46},
		{"enlarges small source", 20, 10, 60, 60, 60, 30},
		{"aspect already matches box", 64, 32, 40, 20, 40, 20},
		{"non-square box, width bound", 100, 100, 30, 60, 30, 30},
		{"extreme ratio never collapses to zero", 1000, 1, 10, 10, 10, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := fitMarkerSize(tc.srcW, tc.srcH, tc.boxW, tc.boxH)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("fitMarkerSize(%d×%d into %d×%d) = %d×%d, want %d×%d",
					tc.srcW, tc.srcH, tc.boxW, tc.boxH, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

// TestDrawMarkerPreservesAspectRatio draws a 128×49 marker into a
// 60×60 box at scale 1 and 2 and checks the painted extent is the
// fitted size, centred on the marker's point.
func TestDrawMarkerPreservesAspectRatio(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("Markers", 0o755); err != nil {
		t.Fatal(err)
	}
	src := image.NewNRGBA(image.Rect(0, 0, 128, 49))
	red := color.NRGBA{R: 255, A: 255}
	for y := 0; y < 49; y++ {
		for x := 0; x < 128; x++ {
			src.SetNRGBA(x, y, red)
		}
	}
	f, err := os.Create(filepath.Join("Markers", "wide.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, src); err != nil {
		t.Fatal(err)
	}
	f.Close()

	for _, scale := range []uint8{1, 2} {
		staticMap := models.StaticMap{Latitude: 51.5, Longitude: -0.1, Zoom: 15, Width: 200, Height: 200, Scale: scale}
		marker := models.Marker{URL: "wide.png", Width: 60, Height: 60, Latitude: 51.5, Longitude: -0.1}
		canvas := image.NewNRGBA(image.Rect(0, 0, 200*int(scale), 200*int(scale)))

		drawMarkerNRGBA(canvas, staticMap, marker, NewSphericalMercator(), scale)

		painted := image.Rectangle{}
		b := canvas.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if canvas.NRGBAAt(x, y).A > 0 {
					painted = painted.Union(image.Rect(x, y, x+1, y+1))
				}
			}
		}

		wantW, wantH := 60*int(scale), map[uint8]int{1: 23, 2: 46}[scale]
		if painted.Dx() != wantW || painted.Dy() != wantH {
			t.Errorf("scale=%d: painted %d×%d, want %d×%d (aspect preserved)",
				scale, painted.Dx(), painted.Dy(), wantW, wantH)
		}
		cx, cy := b.Dx()/2, b.Dy()/2
		if mid := (painted.Min.X + painted.Max.X) / 2; abs(mid-cx) > 1 {
			t.Errorf("scale=%d: horizontal centre %d, want %d±1", scale, mid, cx)
		}
		if mid := (painted.Min.Y + painted.Max.Y) / 2; abs(mid-cy) > 1 {
			t.Errorf("scale=%d: vertical centre %d, want %d±1", scale, mid, cy)
		}
	}
}

package renderer

import (
	"math"
	"testing"
)

func TestTileToViewport(t *testing.T) {
	cases := []struct {
		name             string
		z, x, y          int
		scale            uint8
		wantZoom         float64
		wantLng, wantLat float64
	}{
		{
			name: "z0 only tile",
			z:    0, x: 0, y: 0, scale: 1,
			wantZoom: 0,
			wantLng:  0, wantLat: 0,
		},
		{
			name: "z1 top-left",
			z:    1, x: 0, y: 0, scale: 1,
			wantZoom: 1,
			wantLng:  -90,
			wantLat:  66.51326044311186,
		},
		{
			name: "z14 london-ish",
			z:    14, x: 8188, y: 5448, scale: 1,
			wantZoom: 14,
			// For tile (8188, 5448) at z=14:
			//   n = 2^14 = 16384
			//   nx = 8188.5 / 16384 = 0.499786376953125
			//   lng = nx*360 - 180 = -0.076904296875
			//   ny = 5448.5 / 16384 ≈ 0.33255004...
			//   lat = atan(sinh(π*(1-2*ny))) * 180/π ≈ 51.501904...
			wantLng: -0.076904296875,
			wantLat: 51.501904107618124,
		},
		{
			name: "z14 scale 2 sets Scale but Width stays constant",
			z:    14, x: 0, y: 0, scale: 2,
			wantZoom: 14,
			// lng/lat not asserted for this case (guard below)
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TileToViewport(tc.z, tc.x, tc.y, tc.scale)
			if got.Zoom != tc.wantZoom {
				t.Errorf("zoom: got %v, want %v", got.Zoom, tc.wantZoom)
			}
			// Width/Height are always the logical tile size; actual
			// pixel dimensions are Width*Scale, applied by the backend.
			if got.Width != TileSizePx || got.Height != TileSizePx {
				t.Errorf("width/height: got %dx%d, want %dx%d",
					got.Width, got.Height, TileSizePx, TileSizePx)
			}
			if got.Scale != tc.scale {
				t.Errorf("scale: got %d, want %d", got.Scale, tc.scale)
			}
			if tc.wantLng != 0 || tc.wantLat != 0 {
				if math.Abs(got.Longitude-tc.wantLng) > 1e-6 {
					t.Errorf("lng: got %v, want %v", got.Longitude, tc.wantLng)
				}
				if math.Abs(got.Latitude-tc.wantLat) > 1e-6 {
					t.Errorf("lat: got %v, want %v", got.Latitude, tc.wantLat)
				}
			}
			if got.Bearing != 0 || got.Pitch != 0 {
				t.Errorf("tile renders must be zero-rotation")
			}
		})
	}
}

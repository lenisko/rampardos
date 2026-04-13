package renderer

import "math"

// TileSizePx is the logical width/height (in pixels) of a rendered
// tile. MapLibre GL Native uses 512 as its base tile size (not the
// traditional web-map 256), so a 512px viewport at zoom z covers
// exactly one standard XYZ tile's geographic extent. The stitching
// code in static_map.go must use this same value when computing tile
// grid dimensions and offsets.
const TileSizePx = 512

// TileToViewport converts a tile address (z, x, y) and DPR scale into
// the ViewportRequest that produces the equivalent raster. The
// returned viewport is:
//
//   - Centred on the tile's centre in lng/lat (web-mercator / EPSG:3857)
//   - Sized to TileSizePx logical pixels on each side (constant,
//     regardless of scale — the Scale field is the DPR and the backend
//     multiplies Width × Scale to get actual pixels, so a scale=2 tile
//     renders as 512 actual pixels on each side)
//   - At integer zoom, with zero bearing and pitch
//
// Conversion uses the standard spherical Mercator XYZ tile scheme.
// Caller precondition: 0 ≤ z ≤ 22, 0 ≤ x,y < 2^z. The caller must
// set StyleID and Format on the returned ViewportRequest before
// dispatching to a Renderer; this helper is pure geometry.
//
// A scale of 0 is normalised to 1.
func TileToViewport(z, x, y int, scale uint8) ViewportRequest {
	n := math.Exp2(float64(z))
	nx := (float64(x) + 0.5) / n
	ny := (float64(y) + 0.5) / n

	lng := nx*360.0 - 180.0
	latRad := math.Atan(math.Sinh(math.Pi * (1 - 2*ny)))
	lat := latRad * 180.0 / math.Pi

	if scale == 0 {
		scale = 1
	}

	return ViewportRequest{
		Longitude: lng,
		Latitude:  lat,
		Zoom:      float64(z),
		Width:     TileSizePx,
		Height:    TileSizePx,
		Bearing:   0,
		Pitch:     0,
		Scale:     scale,
	}
}

package renderer

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
)

// styleZoomOffset returns log2(tileSize/256) for the style's primary
// source. MapLibre Native interprets a ViewportRequest's zoom in the
// source's tile-size convention; the web-map convention (what poracle
// and every public tile URL uses) is 256. So for a 512-tileSize style,
// MapLibre's zoom=14 render shows twice the detail of the web-map
// zoom=14 render. Subtracting the offset from the zoom we send to
// MapLibre produces output that matches the caller's web-map intent.
//
// Only applied on the viewport path (RenderViewportImage). The tile
// path already sends width=TileSizePx=512 to MapLibre; for a 512-tile
// style that yields exactly one standard web tile at zoom Z, so no
// adjustment is needed there.
//
// Falls back to 1.0 (the MapLibre vector default of tileSize=512) if
// the style cannot be parsed — the overwhelming majority of styles.
func styleZoomOffset(src []byte) float64 {
	var style struct {
		Sources map[string]struct {
			Type     string `json:"type"`
			TileSize *int   `json:"tileSize,omitempty"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(src, &style); err != nil {
		return 1.0
	}
	for _, s := range style.Sources {
		ts := sourceTileSize(s.Type, s.TileSize)
		if ts <= 0 {
			continue
		}
		return math.Log2(float64(ts) / 256.0)
	}
	return 1.0
}

// sourceTileSize returns the effective tileSize for a source given its
// explicit tileSize (nil if unset) and type. MapLibre defaults:
// vector → 512, raster → 256, raster-dem → 512. Unknown types are
// treated as vector.
func sourceTileSize(sourceType string, explicit *int) int {
	if explicit != nil {
		return *explicit
	}
	switch sourceType {
	case "raster":
		return 256
	default:
		return 512
	}
}

// PrepareStyle takes the on-disk style.json for a given style and
// returns a modified version where sprite, glyphs, and mbtiles vector
// source URLs point at absolute local paths the render worker can
// resolve via its `request` callback without any network access.
//
// Rewrites applied:
//
//   - sprite: "{styleUrl}/sprite"  ->  "file://<stylesDir>/<id>/sprite"
//   - glyphs: "{fontUrl}/{fontstack}/{range}.pbf"  ->
//     "file://<fontsDir>/{fontstack}/{range}.pbf"
//     (the {fontstack} and {range} tokens are preserved — they are
//     resolved per-request by the worker's callback, not here)
//   - sources.*.url: "mbtiles://<name>"  ->
//     "mbtiles://<absolute-mbtiles-file>"
//     (the <name> part is ignored — rampardos uses a single combined
//     mbtiles per deployment)
//
// Any http(s)// URLs are left untouched — they are assumed to be
// legitimate CDN references, not placeholders.
func PrepareStyle(id string, src []byte, cfg Config) ([]byte, error) {
	var style map[string]any
	if err := json.Unmarshal(src, &style); err != nil {
		return nil, fmt.Errorf("renderer: parse style.json: %w", err)
	}

	if sprite, ok := style["sprite"].(string); ok && !isHTTP(sprite) {
		style["sprite"] = "file://" + filepath.Join(cfg.StylesDir, id, "sprite")
	}

	if glyphs, ok := style["glyphs"].(string); ok && !isHTTP(glyphs) {
		// Strip the placeholder prefix, keep the fontstack/range tokens.
		style["glyphs"] = "file://" + filepath.Join(cfg.FontsDir, "{fontstack}", "{range}.pbf")
	}

	if sources, ok := style["sources"].(map[string]any); ok {
		for _, v := range sources {
			src, ok := v.(map[string]any)
			if !ok {
				continue
			}
			url, ok := src["url"].(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(url, "mbtiles://") {
				src["url"] = "mbtiles://" + cfg.MbtilesFile
			}
		}
	}

	return json.Marshal(style)
}

func isHTTP(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

package renderer

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
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
	names := make([]string, 0, len(style.Sources))
	for name := range style.Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ts := sourceTileSize(style.Sources[name].Type, style.Sources[name].TileSize)
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
//   - sources.*.url for vector sources (or sources with no explicit
//     type, which MapLibre defaults to vector) is always rewritten to
//     "mbtiles://<absolute-mbtiles-file>", regardless of the incoming
//     scheme. This collapses several upstream variants — "mbtiles://X",
//     "https://api.maptiler.com/tiles/v3/tiles.json?key={key}",
//     "mapbox://openmaptiles.X" — onto the operator's combined
//     mbtiles, which is the only data source the local renderer has.
//
// Raster, raster-dem, geojson, image, and video source URLs are left
// alone: they may legitimately point at remote CDNs / tile services
// (e.g. a hillshade overlay), and the worker's http(s) branch fetches
// them on demand.
//
// http(s) URLs for sprite and glyphs are left untouched — they are
// assumed to be legitimate CDN references and the worker now fetches
// and caches them.
func PrepareStyle(id string, src []byte, cfg Config) ([]byte, error) {
	// Validate id against path traversal: must be non-empty and contain
	// only [A-Za-z0-9_-] with no path separators, "..", or null bytes.
	if id == "" {
		return nil, fmt.Errorf("renderer: style id must not be empty")
	}
	if strings.Contains(id, "..") || strings.Contains(id, "/") ||
		strings.Contains(id, "\\") || strings.Contains(id, "\x00") {
		return nil, fmt.Errorf("renderer: style id contains invalid characters: %q", id)
	}
	for _, c := range id {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return nil, fmt.Errorf("renderer: style id contains invalid characters: %q", id)
		}
	}

	var style map[string]any
	if err := json.Unmarshal(src, &style); err != nil {
		return nil, fmt.Errorf("renderer: parse style.json: %w", err)
	}

	if sprite, ok := style["sprite"].(string); ok && !isHTTP(sprite) {
		style["sprite"] = "file://" + filepath.Join(cfg.StylesDir, id, "sprite")
	} else if arr, ok := style["sprite"].([]any); ok {
		for _, entry := range arr {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if u, ok := m["url"].(string); ok && !isHTTP(u) {
				m["url"] = "file://" + filepath.Join(cfg.StylesDir, id, "sprite")
			}
		}
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
			if _, ok := src["url"].(string); !ok {
				continue
			}
			// Vector sources (and sources with no explicit type, per
			// MapLibre's vector default) are always served from the
			// local combined mbtiles. This covers upstream variants
			// that would otherwise reach the worker as raw URLs —
			// MapTiler tiles.json with an unsubstituted {key}, mapbox://
			// references, etc.
			srcType, _ := src["type"].(string)
			if srcType == "vector" || srcType == "" {
				src["url"] = "mbtiles://" + cfg.MbtilesFile
			}
		}
	}

	return json.Marshal(style)
}

func isHTTP(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

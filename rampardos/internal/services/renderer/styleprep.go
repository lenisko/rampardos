package renderer

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

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

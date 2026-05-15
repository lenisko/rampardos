package renderer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareStyle(t *testing.T) {
	src, err := os.ReadFile("testdata/style_unresolved.json")
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		StylesDir:   "/tileserver/styles",
		FontsDir:    "/tileserver/fonts",
		MbtilesFile: "/tileserver/data/Combined.mbtiles",
	}

	out, err := PrepareStyle("example", src, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("prepared style is not valid JSON: %v", err)
	}

	// Sprite should be absolute file URL into the style directory.
	wantSprite := "file://" + filepath.Join("/tileserver/styles", "example", "sprite")
	if got, _ := parsed["sprite"].(string); got != wantSprite {
		t.Errorf("sprite: got %q, want %q", got, wantSprite)
	}

	// Glyphs should retain the {fontstack}/{range} placeholders but
	// have the base rewritten to the fonts directory.
	wantGlyphs := "file://" + filepath.Join("/tileserver/fonts", "{fontstack}", "{range}.pbf")
	if got, _ := parsed["glyphs"].(string); got != wantGlyphs {
		t.Errorf("glyphs: got %q, want %q", got, wantGlyphs)
	}

	// Sources with url of mbtiles://NAME should be rewritten to
	// mbtiles://<absolute-mbtiles-file>.
	sources, ok := parsed["sources"].(map[string]any)
	if !ok {
		t.Fatal("sources missing or wrong type")
	}
	openmaptiles, ok := sources["openmaptiles"].(map[string]any)
	if !ok {
		t.Fatal("openmaptiles source missing")
	}
	wantURL := "mbtiles://" + "/tileserver/data/Combined.mbtiles"
	if got, _ := openmaptiles["url"].(string); got != wantURL {
		t.Errorf("source url: got %q, want %q", got, wantURL)
	}
}

func TestPrepareStyleRejectsInvalidJSON(t *testing.T) {
	_, err := PrepareStyle("example", []byte("{not json"), Config{})
	if err == nil {
		t.Errorf("expected error for invalid JSON, got nil")
	}
}

func TestPrepareStyleKeepsHTTPGlyphsUnchanged(t *testing.T) {
	// A style that already has an http:// glyphs URL should not be
	// rewritten — it's a legitimate CDN reference, not a placeholder.
	src := []byte(`{"version":8,"glyphs":"https://example.com/{fontstack}/{range}.pbf","sources":{},"layers":[]}`)
	out, err := PrepareStyle("x", src, Config{FontsDir: "/fonts"})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if got := parsed["glyphs"].(string); got != "https://example.com/{fontstack}/{range}.pbf" {
		t.Errorf("http glyphs was rewritten: %q", got)
	}
}

// Vector source URLs must be rewritten to the local mbtiles even
// when the incoming URL is an HTTP TileJSON (the form MapTiler's
// download-style export uses) or has an unsubstituted {key}
// placeholder. The previous logic only rewrote mbtiles:// URLs and
// left these alone, which led to runtime "HTTP 403" failures when the
// worker tried to fetch the URL directly.
func TestPrepareStyleRewritesAllVectorSources(t *testing.T) {
	cfg := Config{StylesDir: "/s", FontsDir: "/f", MbtilesFile: "/d/Combined.mbtiles"}
	wantURL := "mbtiles:///d/Combined.mbtiles"

	cases := []struct {
		name string
		body string
	}{
		{
			name: "https TileJSON with {key} placeholder",
			body: `{"version":8,"sources":{"omt":{"type":"vector","url":"https://api.maptiler.com/tiles/v3/tiles.json?key={key}"}}}`,
		},
		{
			name: "http TileJSON",
			body: `{"version":8,"sources":{"omt":{"type":"vector","url":"http://example.com/tiles.json"}}}`,
		},
		{
			name: "mapbox:// reference",
			body: `{"version":8,"sources":{"omt":{"type":"vector","url":"mapbox://openmaptiles.v3"}}}`,
		},
		{
			name: "mbtiles:// placeholder (existing behaviour)",
			body: `{"version":8,"sources":{"omt":{"type":"vector","url":"mbtiles://openmaptiles"}}}`,
		},
		{
			name: "no type field, defaults to vector",
			body: `{"version":8,"sources":{"omt":{"url":"https://api.maptiler.com/tiles/v3/tiles.json?key=X"}}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := PrepareStyle("x", []byte(tc.body), cfg)
			if err != nil {
				t.Fatal(err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatal(err)
			}
			got, _ := parsed["sources"].(map[string]any)["omt"].(map[string]any)["url"].(string)
			if got != wantURL {
				t.Errorf("source url: got %q, want %q", got, wantURL)
			}
		})
	}
}

// Raster, raster-dem, geojson, image, and video sources are NOT
// rewritten: they may legitimately point at remote CDNs (e.g. a
// hillshade overlay served from a separate tile service).
func TestPrepareStyleLeavesNonVectorSourcesAlone(t *testing.T) {
	cfg := Config{StylesDir: "/s", FontsDir: "/f", MbtilesFile: "/d/Combined.mbtiles"}

	cases := []struct {
		name      string
		body      string
		wantURL   string
		sourceKey string
	}{
		{
			name:      "raster source",
			body:      `{"version":8,"sources":{"hill":{"type":"raster","url":"https://hillshade.example.com/tiles.json","tileSize":256}}}`,
			wantURL:   "https://hillshade.example.com/tiles.json",
			sourceKey: "hill",
		},
		{
			name:      "raster-dem source",
			body:      `{"version":8,"sources":{"terrain":{"type":"raster-dem","url":"https://terrain.example.com/tiles.json"}}}`,
			wantURL:   "https://terrain.example.com/tiles.json",
			sourceKey: "terrain",
		},
		{
			name:      "geojson source",
			body:      `{"version":8,"sources":{"places":{"type":"geojson","url":"https://example.com/places.geojson"}}}`,
			wantURL:   "https://example.com/places.geojson",
			sourceKey: "places",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := PrepareStyle("x", []byte(tc.body), cfg)
			if err != nil {
				t.Fatal(err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatal(err)
			}
			got, _ := parsed["sources"].(map[string]any)[tc.sourceKey].(map[string]any)["url"].(string)
			if got != tc.wantURL {
				t.Errorf("source url: got %q, want %q (rewrite should only touch vector sources)", got, tc.wantURL)
			}
		})
	}
}

func TestStyleZoomOffset(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{
			name: "vector source with default tileSize",
			body: `{"sources":{"omt":{"type":"vector","url":"mbtiles://x"}}}`,
			want: 1.0,
		},
		{
			name: "explicit tileSize=256",
			body: `{"sources":{"s":{"type":"vector","url":"mbtiles://x","tileSize":256}}}`,
			want: 0.0,
		},
		{
			name: "explicit tileSize=512",
			body: `{"sources":{"s":{"type":"vector","url":"mbtiles://x","tileSize":512}}}`,
			want: 1.0,
		},
		{
			name: "explicit tileSize=1024",
			body: `{"sources":{"s":{"type":"vector","url":"mbtiles://x","tileSize":1024}}}`,
			want: 2.0,
		},
		{
			name: "raster source defaults to 256",
			body: `{"sources":{"r":{"type":"raster","tiles":["http://x/{z}/{x}/{y}.png"]}}}`,
			want: 0.0,
		},
		{
			name: "unparseable style falls back to 512",
			body: `not json`,
			want: 1.0,
		},
		{
			name: "no sources falls back to 512",
			body: `{"version":8,"name":"empty"}`,
			want: 1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := styleZoomOffset([]byte(tc.body))
			if got != tc.want {
				t.Errorf("styleZoomOffset(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

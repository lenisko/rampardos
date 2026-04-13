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

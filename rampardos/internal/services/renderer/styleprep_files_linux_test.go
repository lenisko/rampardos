package renderer

import (
	"os"
	"path/filepath"
	"testing"
)

// A style whose text-font lists several fonts needs a composite font
// directory named after the whole stack. The Node backend composed it
// when loading a pool; the in-process loader dropped the step, so every
// render of such a style failed on its glyphs and retired the worker.
func TestPrepareStyleFilesComposesFontstacks(t *testing.T) {
	dir := t.TempDir()
	fonts := filepath.Join(dir, "Fonts")
	for _, font := range []string{"Metropolis Regular", "Noto Sans Regular"} {
		if err := os.MkdirAll(filepath.Join(fonts, font), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fonts, font, "0-255.pbf"), makePBF(font, "0-255", [][2]uint32{{65, 10}}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	styles := filepath.Join(dir, "Styles")
	if err := os.MkdirAll(filepath.Join(styles, "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	style := `{"version":8,"glyphs":"{fontstack}/{range}.pbf","sources":{},"layers":[
		{"id":"lbl","type":"symbol","layout":{"text-font":["Metropolis Regular","Noto Sans Regular"]}}]}`
	if err := os.WriteFile(filepath.Join(styles, "s", "style.json"), []byte(style), 0o644); err != nil {
		t.Fatal(err)
	}

	_, prepared, err := prepareStyleFiles(Config{StylesDir: styles, FontsDir: fonts}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared); err != nil {
		t.Fatalf("prepared style not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fonts, "Metropolis Regular,Noto Sans Regular", "0-255.pbf")); err != nil {
		t.Fatalf("composite fontstack not composed: %v", err)
	}
}

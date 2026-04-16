package services

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestAnalyzeUsageReadsDirectoryStyleJSON pins the path convention
// after the flat-layout migration: analyzeUsage must read
// <folder>/<id>/style.json, not the legacy flat <folder>/<id>.json.
// Once promoteFlatStyleFiles has run at startup, only the directory
// path is populated; the pre-migration path no longer exists.
func TestAnalyzeUsageReadsDirectoryStyleJSON(t *testing.T) {
	folder := t.TempDir()
	styleDir := filepath.Join(folder, "analyze")
	if err := os.MkdirAll(styleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	style := []byte(`{
		"version": 8,
		"layers": [
			{"id":"l1","type":"symbol","layout":{"text-font":["Roboto Regular"],"icon-image":"marker-15"}},
			{"id":"l2","type":"background","paint":{"background-pattern":"grid"}}
		]
	}`)
	if err := os.WriteFile(filepath.Join(styleDir, "style.json"), style, 0o644); err != nil {
		t.Fatal(err)
	}

	sc := &StylesController{folder: folder}
	got := sc.analyzeUsage("analyze")

	if !slices.Contains(got.fonts, "Roboto Regular") {
		t.Errorf("expected Roboto Regular in fonts, got %v", got.fonts)
	}
	wantIcons := []string{"marker-15", "grid"}
	for _, icon := range wantIcons {
		if !slices.Contains(got.icons, icon) {
			t.Errorf("expected %q in icons, got %v", icon, got.icons)
		}
	}
}

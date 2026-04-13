//go:build renderer_integration
// +build renderer_integration

package renderer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lenisko/rampardos/internal/models"
)

// TestIntegrationRealWorker exercises the full NodePoolRenderer against
// a real Node worker spawned with `@maplibre/maplibre-gl-native`. It
// uses a minimal background-only style so the test does not need real
// tile data inside the mbtiles file.
//
// Run with: go test -tags renderer_integration ./internal/services/renderer/ -v
//
// Prerequisites:
//   - rampardos-render-worker/node_modules populated via `npm install`
//   - Node on PATH
func TestIntegrationRealWorker(t *testing.T) {
	workerDir := findWorkerDir(t)

	// Write a minimal style that requires no sources, sprites, or glyphs.
	tmp := t.TempDir()
	styleDir := filepath.Join(tmp, "bg")
	if err := os.MkdirAll(styleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stylePath := filepath.Join(styleDir, "style.json")
	style := []byte(`{"version":8,"name":"bg","sources":{},"layers":[{"id":"bg","type":"background","paint":{"background-color":"#ff8800"}}]}`)
	if err := os.WriteFile(stylePath, style, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a minimal valid mbtiles file so better-sqlite3 can open it.
	// better-sqlite3 with fileMustExist requires a real SQLite DB with
	// the `tiles` table the worker queries at startup.
	mbtilesPath := filepath.Join(tmp, "empty.mbtiles")
	if err := createMinimalMbtiles(t, mbtilesPath); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Backend:        "node-pool",
		NodeBinary:     "node",
		WorkerScript:   filepath.Join(workerDir, "render-worker.js"),
		PoolSize:       1,
		RenderTimeout:  15 * time.Second,
		WorkerLifetime: 100,
		StartupTimeout: 15 * time.Second,
		StylesDir:      tmp,
		FontsDir:       tmp,
		MbtilesFile:    mbtilesPath,
		DiscoverStyles: func() ([]string, error) { return []string{"bg"}, nil },
	}

	// Build a SpawnFactory that launches the real Node worker with the
	// CLI args render-worker.js expects.
	sf := func(styleID, preparedStylePath string) func() (*worker, error) {
		return func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:  cfg.NodeBinary,
				script:  cfg.WorkerScript,
				styleID: styleID,
				scriptArgs: []string{
					"--style-id", styleID,
					"--style-path", preparedStylePath,
					"--mbtiles", cfg.MbtilesFile,
					"--styles-dir", cfg.StylesDir,
					"--fonts-dir", cfg.FontsDir,
				},
				handshakeTimeout: cfg.StartupTimeout,
			})
		}
	}

	r, err := NewNodePoolRenderer(cfg, sf)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := r.Render(ctx, Request{
		StyleID: "bg",
		Z:       0, X: 0, Y: 0,
		Scale:  1,
		Format: models.ImageFormatPNG,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's a valid PNG.
	if len(out) < 8 || string(out[1:4]) != "PNG" {
		t.Errorf("not a PNG: %x", out[:min(16, len(out))])
	}
	// Spot-check size.
	if len(out) < 100 {
		t.Errorf("PNG suspiciously small: %d bytes", len(out))
	}
	t.Logf("render produced %d-byte PNG", len(out))
}

func findWorkerDir(t *testing.T) string {
	t.Helper()
	root := findRepoRoot(t)
	candidate := filepath.Join(root, "rampardos-render-worker")
	if _, err := os.Stat(filepath.Join(candidate, "render-worker.js")); err != nil {
		t.Skipf("rampardos-render-worker not found at %s: %v", candidate, err)
	}
	if _, err := os.Stat(filepath.Join(candidate, "node_modules", "@maplibre", "maplibre-gl-native")); err != nil {
		t.Skipf("@maplibre/maplibre-gl-native not installed — run `npm install` in %s", candidate)
	}
	return candidate
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Tests run from rampardos/internal/services/renderer.
	// Repo root is four levels up.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(cwd, "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// createMinimalMbtiles uses the sqlite3 CLI to create a valid mbtiles
// file with the `tiles` table the render worker expects. The table is
// empty — the background-only style never queries tiles.
func createMinimalMbtiles(t *testing.T, path string) error {
	t.Helper()
	sql := `CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
CREATE TABLE metadata (name TEXT, value TEXT);
INSERT INTO metadata VALUES ('name', 'empty');
INSERT INTO metadata VALUES ('format', 'pbf');`
	cmd := exec.Command("sqlite3", path, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3: %w: %s", err, out)
	}
	return nil
}

//go:build renderer_integration

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

// Integration tests for the renderer, parameterised over the backend.
//
// TestIntegrationNode exercises NodePoolRenderer against a real Node
// worker spawned with @maplibre/maplibre-gl-native. TestIntegrationGo
// (in integration_mln_ffi_test.go, gated by the mln_ffi build tag)
// exercises GoPoolRenderer against the maplibre-native-go binding.
// Both share the fixtures and assertions defined here so the two paths
// stay observably equivalent at the Renderer interface boundary.
//
// Run Node only:
//   go test -tags renderer_integration ./internal/services/renderer/ -v
//
// Run both Node and Go (requires libmaplibre-native-c.so available
// via pkg-config; see scripts/build-mln-ffi.sh):
//   go test -tags 'renderer_integration mln_ffi' ./internal/services/renderer/ -v

// TestIntegrationNode exercises NodePoolRenderer against a real
// `node render-worker.js` spawned with @maplibre/maplibre-gl-native.
//
// Prerequisites:
//   - rampardos-render-worker/node_modules populated via `npm install`
//   - Node on PATH
func TestIntegrationNode(t *testing.T) {
	workerDir := findWorkerDir(t)

	cfg := setupIntegrationFixtures(t)
	cfg.Backend = "node-pool"
	cfg.NodeBinary = "node"
	cfg.WorkerScript = filepath.Join(workerDir, "render-worker.js")
	cfg.WorkerLifetime = 100

	r, err := NewNodePoolRenderer(cfg, DefaultSpawnFactory(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	runRenderAssertions(t, r)
}

// setupIntegrationFixtures creates a temp dir with a minimal background-
// only style and an empty-but-valid mbtiles file, returning a Config
// that points at them. The caller adds backend-specific fields
// (NodeBinary/WorkerScript for Node, none for Go) and constructs the
// renderer.
func setupIntegrationFixtures(t *testing.T) Config {
	t.Helper()

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

	// Minimal valid mbtiles. Both backends open the file at startup;
	// the background-only style never reads tiles, so the table can be
	// empty as long as the schema exists.
	mbtilesPath := filepath.Join(tmp, "empty.mbtiles")
	if err := createMinimalMbtiles(t, mbtilesPath); err != nil {
		t.Fatal(err)
	}

	return Config{
		PoolSize:       1,
		StylePoolSize:  1,
		RenderTimeout:  15 * time.Second,
		StartupTimeout: 30 * time.Second,
		StylesDir:      tmp,
		FontsDir:       tmp,
		MbtilesFile:    mbtilesPath,
		DiscoverStyles: func() ([]string, error) { return []string{"bg"}, nil },
	}
}

// runRenderAssertions exercises r.Render against the "bg" style fixture
// at z=0 x=0 y=0 and verifies the output is a non-trivial PNG. Shared
// across backends so a regression visible at the interface (wrong format,
// empty buffer, encoder failure) trips the same assertion regardless of
// which renderer produced it.
func runRenderAssertions(t *testing.T, r Renderer) {
	t.Helper()

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

	// PNG signature: 89 50 4E 47 0D 0A 1A 0A. Bytes 1-3 are "PNG".
	if len(out) < 8 || string(out[1:4]) != "PNG" {
		head := out
		if len(head) > 16 {
			head = head[:16]
		}
		t.Errorf("not a PNG: %x", head)
	}
	// Spot-check size: a real render of a single-color tile is ~hundreds
	// of bytes; anything sub-100 is a missing-payload smell.
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
// file with the `tiles` table both backends expect. The table is empty
// — the background-only style never queries tiles.
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

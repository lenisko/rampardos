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

// Shared fixtures and assertions for the renderer integration tests.
// The only backend is the in-process Go renderer; TestIntegrationGo lives
// in integration_mln_ffi_test.go because it additionally needs the
// mln_ffi build tag and libmaplibre-native-c.so.
//
// Run (requires libmaplibre-native-c.so via pkg-config; see
// scripts/build-mln-ffi.sh):
//   go test -tags 'renderer_integration mln_ffi' ./internal/services/renderer/ -v

// setupIntegrationFixtures creates a temp dir with a minimal background-
// only style and an empty-but-valid mbtiles file, returning a Config
// that points at them. The caller sets Backend and constructs the
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

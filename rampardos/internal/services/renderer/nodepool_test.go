package renderer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lenisko/rampardos/internal/models"
)

// writeTestStyle creates a minimal style.json on disk that PrepareStyle
// can rewrite. Returns the path to the styles directory root.
func writeTestStyle(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	styleDir := filepath.Join(dir, id)
	if err := os.MkdirAll(styleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	style := map[string]any{
		"version": 8,
		"name":    id,
		"sources": map[string]any{},
		"layers":  []any{},
	}
	b, _ := json.Marshal(style)
	if err := os.WriteFile(filepath.Join(styleDir, "style.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNodePoolRendererRenderReturnsBytes(t *testing.T) {
	stylesDir := writeTestStyle(t, "basic")

	npr, err := NewNodePoolRenderer(Config{
		StylesDir:      stylesDir,
		FontsDir:       t.TempDir(),
		MbtilesFile:    "/tmp/fake.mbtiles",
		PoolSize:       1,
		WorkerLifetime: 100,
		RenderTimeout:  5 * time.Second,
		StartupTimeout: 2 * time.Second,
		DiscoverStyles: func() ([]string, error) { return []string{"basic"}, nil },
	}, func(styleID, preparedPath string) func() (*worker, error) {
		return func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:           "bash",
				script:           "testdata/fake-worker-ok.sh",
				styleID:          styleID,
				handshakeTimeout: 2 * time.Second,
			})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer npr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := npr.Render(ctx, Request{
		StyleID: "basic",
		Z:       14,
		X:       8192,
		Y:       8192,
		Scale:   1,
		Format:  models.ImageFormatPNG,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected non-empty render output")
	}
}

func TestNodePoolRendererUnknownStyle(t *testing.T) {
	stylesDir := writeTestStyle(t, "basic")

	npr, err := NewNodePoolRenderer(Config{
		StylesDir:      stylesDir,
		FontsDir:       t.TempDir(),
		MbtilesFile:    "/tmp/fake.mbtiles",
		PoolSize:       1,
		WorkerLifetime: 100,
		RenderTimeout:  5 * time.Second,
		StartupTimeout: 2 * time.Second,
		DiscoverStyles: func() ([]string, error) { return []string{"basic"}, nil },
	}, func(styleID, preparedPath string) func() (*worker, error) {
		return func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:           "bash",
				script:           "testdata/fake-worker-ok.sh",
				styleID:          styleID,
				handshakeTimeout: 2 * time.Second,
			})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer npr.Close()

	ctx := context.Background()
	_, err = npr.Render(ctx, Request{
		StyleID: "nonexistent",
		Z:       14,
		X:       8192,
		Y:       8192,
		Scale:   1,
		Format:  models.ImageFormatPNG,
	})
	if err == nil {
		t.Fatal("expected error for unknown style, got nil")
	}
}


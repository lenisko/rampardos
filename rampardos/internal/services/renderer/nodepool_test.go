package renderer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	}, func(styleID, preparedPath string, ratio int) func() (*worker, error) {
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

// TestReloadStylesPreservesConcurrentlyCreatedPool pins the invariant
// that a pool inserted via getOrCreatePool during ReloadStyles's
// outside-lock loop must survive the final swap. Before the fix
// ReloadStyles unconditionally replaced npr.pools with the newly built
// map, dropping (and then closing) any pool added in the window.
func TestReloadStylesPreservesConcurrentlyCreatedPool(t *testing.T) {
	stylesDir := t.TempDir()
	for _, id := range []string{"A", "B"} {
		styleDir := filepath.Join(stylesDir, id)
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
	}

	var aSpawnCount atomic.Int32
	gateAReload := make(chan struct{})
	gateB := make(chan struct{})

	sf := func(styleID, preparedPath string, ratio int) func() (*worker, error) {
		return func() (*worker, error) {
			switch styleID {
			case "A":
				// First A spawn (pre-create) returns immediately. The
				// second (ReloadStyles) blocks until the test releases
				// gateAReload so we can race getOrCreatePool("B") into
				// the window.
				if aSpawnCount.Add(1) > 1 {
					<-gateAReload
				}
			case "B":
				<-gateB
			}
			return spawnWorker(workerArgs{
				binary:           "bash",
				script:           "testdata/fake-worker-ok.sh",
				styleID:          styleID,
				handshakeTimeout: 2 * time.Second,
			})
		}
	}

	npr, err := NewNodePoolRenderer(Config{
		StylesDir:      stylesDir,
		FontsDir:       t.TempDir(),
		MbtilesFile:    "/tmp/fake.mbtiles",
		PoolSize:       1,
		WorkerLifetime: 100,
		RenderTimeout:  5 * time.Second,
		StartupTimeout: 2 * time.Second,
		DiscoverStyles: func() ([]string, error) { return []string{"A", "B"}, nil },
	}, sf)
	if err != nil {
		t.Fatal(err)
	}
	defer npr.Close()

	// Pre-create pool A so ReloadStyles has an active key to snapshot.
	if _, err := npr.getOrCreatePool("A", 1); err != nil {
		t.Fatalf("pre-create A: %v", err)
	}

	// Kick off ReloadStyles. Snapshot={A} → enter outside-lock loop →
	// loadPool("A") → spawn A → blocks on gateAReload.
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- npr.ReloadStyles(context.Background())
	}()

	// Wait for ReloadStyles to be blocked inside the reloaded A spawn.
	time.Sleep(50 * time.Millisecond)

	// Race getOrCreatePool("B") into the window. Takes write lock,
	// loadPool("B") → spawn B → blocks on gateB.
	type result struct {
		pool *stylePool
		err  error
	}
	bResult := make(chan result, 1)
	go func() {
		p, err := npr.getOrCreatePool("B", 1)
		bResult <- result{p, err}
	}()

	// Give getOrCreatePool time to reach its blocked spawn, then let
	// B finish creating before the reload swap runs.
	time.Sleep(50 * time.Millisecond)
	close(gateB)

	var pB *stylePool
	select {
	case r := <-bResult:
		if r.err != nil {
			t.Fatalf("getOrCreatePool B: %v", r.err)
		}
		pB = r.pool
	case <-time.After(3 * time.Second):
		t.Fatal("getOrCreatePool B did not complete")
	}

	// Unblock reloaded A so ReloadStyles loop finishes and swaps.
	close(gateAReload)
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("ReloadStyles: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReloadStyles did not complete")
	}

	npr.mu.RLock()
	got := npr.pools["B"]
	npr.mu.RUnlock()
	if got == nil {
		t.Fatal("pool B missing after ReloadStyles — the wholesale swap orphaned it")
	}
	if got != pB {
		t.Fatal("pool B was replaced by ReloadStyles — the concurrently-created pool must be preserved")
	}
}

// TestLoadPoolWritesPreparedStyleAtomically locks in the invariant
// that a reader observing style.prepared.json while loadPool is
// writing must never see a partially-written file. Before the fix
// loadPool called os.WriteFile which performs a truncate-then-write —
// concurrent reloads (or a worker spawned by one loadPool racing the
// write of another) could therefore read truncated or partial JSON.
func TestLoadPoolWritesPreparedStyleAtomically(t *testing.T) {
	stylesDir := t.TempDir()
	styleDir := filepath.Join(stylesDir, "atomic")
	if err := os.MkdirAll(styleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Inflate style.json so prepared.json is large enough that a
	// non-atomic write has a meaningful window for a reader to land
	// in the middle of it.
	layers := make([]any, 0, 400)
	for i := 0; i < 400; i++ {
		layers = append(layers, map[string]any{
			"id":   fmt.Sprintf("layer-%d", i),
			"type": "background",
		})
	}
	style := map[string]any{
		"version": 8,
		"name":    "atomic",
		"sources": map[string]any{},
		"layers":  layers,
	}
	b, _ := json.Marshal(style)
	if err := os.WriteFile(filepath.Join(styleDir, "style.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	npr, err := NewNodePoolRenderer(Config{
		StylesDir:      stylesDir,
		FontsDir:       t.TempDir(),
		MbtilesFile:    "/tmp/fake.mbtiles",
		PoolSize:       1,
		WorkerLifetime: 100,
		RenderTimeout:  time.Second,
		StartupTimeout: time.Second,
		DiscoverStyles: func() ([]string, error) { return []string{"atomic"}, nil },
	}, func(string, string, int) func() (*worker, error) {
		// Spawn intentionally fails — we only care about the side-effect
		// of loadPool writing style.prepared.json, not about workers.
		return func() (*worker, error) {
			return nil, fmt.Errorf("spawn disabled for atomicity test")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer npr.Close()

	preparedPath := filepath.Join(styleDir, "style.prepared.json")

	// Establish the file synchronously so the reader has non-empty
	// content to compare against from the first read onward.
	_, _ = npr.loadPool("atomic", 1)
	if info, err := os.Stat(preparedPath); err != nil || info.Size() == 0 {
		t.Fatalf("prepared file not established: %v", err)
	}

	stop := make(chan struct{})
	var partial atomic.Bool
	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(preparedPath)
			if err != nil {
				// With atomic rename the path always points at a
				// complete file; ENOENT is unexpected once it exists.
				partial.Store(true)
				return
			}
			if len(data) == 0 {
				// Truncate-then-write exposed a zero-length window.
				partial.Store(true)
				return
			}
			var v any
			if jerr := json.Unmarshal(data, &v); jerr != nil {
				partial.Store(true)
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = npr.loadPool("atomic", 1)
		}()
	}
	wg.Wait()
	close(stop)
	readerDone.Wait()

	if partial.Load() {
		t.Fatal("reader observed an incomplete style.prepared.json — write is not atomic")
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
	}, func(styleID, preparedPath string, ratio int) func() (*worker, error) {
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


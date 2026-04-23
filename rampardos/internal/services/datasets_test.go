package services

// NOTE: Tests in this file mutate the package-global cacheCleanerRegistry
// and must not be marked t.Parallel(). If you add parallel tests for
// DatasetsController, put them in a separate file.

import (
	"path/filepath"
	"testing"
)

func TestReloadAfterDatasetChangeDropsTileCache(t *testing.T) {
	// Register a cache cleaner so GetCacheCleaner("Tile") succeeds.
	// NewCacheCleaner registers under filepath.Base(folder), so the folder
	// basename must be literally "Tile".
	zero := uint32(0)
	cleaner := NewCacheCleaner(filepath.Join(t.TempDir(), "Tile"), &zero, &zero, &zero)
	t.Cleanup(func() {
		cacheCleanerRegistryMu.Lock()
		delete(cacheCleanerRegistry, "Tile")
		cacheCleanerRegistryMu.Unlock()
		cleaner.Stop()
	})

	called := false
	dc := &DatasetsController{
		reloadTileserver: func() error {
			called = true
			return nil
		},
	}

	dc.reloadAfterDatasetChange()

	if !called {
		t.Errorf("reloadTileserver callback not invoked")
	}
	if !cleaner.IsDropPending() {
		t.Errorf("expected Cache/Tile drop to be scheduled")
	}
}

func TestReloadAfterDatasetChangeNoCleaner(t *testing.T) {
	// With no "Tile" cleaner registered, the function must not panic.
	// Clear the registry first to isolate from other tests in this package.
	cacheCleanerRegistryMu.Lock()
	delete(cacheCleanerRegistry, "Tile")
	cacheCleanerRegistryMu.Unlock()

	called := false
	dc := &DatasetsController{
		reloadTileserver: func() error {
			called = true
			return nil
		},
	}

	// Must not panic.
	dc.reloadAfterDatasetChange()

	if !called {
		t.Errorf("reloadTileserver callback not invoked")
	}
}

package services

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	tileJoinCommand = "/usr/local/bin/tile-join"
)

// DatasetsController manages mbtiles datasets
type DatasetsController struct {
	folder           string
	listFolder       string
	mu               sync.RWMutex
	uncombined       map[string]bool // datasets that haven't been combined yet
	activeDataset    string          // currently active dataset (empty if combined)
	isCombined       bool            // true if multiple datasets are combined
	reloadTileserver func() error    // callback to reload tileserver
}

// NewDatasetsController creates a new datasets controller
func NewDatasetsController(folder string, reloadTileserver func() error) *DatasetsController {
	listFolder := filepath.Join(folder, "List")
	os.MkdirAll(folder, 0755)
	os.MkdirAll(listFolder, 0755)

	dc := &DatasetsController{
		folder:           folder,
		listFolder:       listFolder,
		uncombined:       make(map[string]bool),
		reloadTileserver: reloadTileserver,
	}

	// Detect current state from symlink
	dc.detectActiveState()

	// Update dataset sizes on startup
	dc.UpdateAllDatasetSizes()

	return dc
}

// detectActiveState checks the Combined.mbtiles symlink to determine current state
func (dc *DatasetsController) detectActiveState() {
	combinedPath := filepath.Join(dc.folder, "Combined.mbtiles")

	// Check if it's a symlink
	linkTarget, err := os.Readlink(combinedPath)
	if err == nil {
		// It's a symlink - extract dataset name
		baseName := filepath.Base(linkTarget)
		dc.activeDataset = strings.TrimSuffix(baseName, ".mbtiles")
		dc.isCombined = false
	} else {
		// Check if Combined.mbtiles exists as a regular file
		if _, err := os.Stat(combinedPath); err == nil {
			dc.isCombined = true
			dc.activeDataset = ""
		}
	}
}

// MarkUncombined marks a dataset as needing to be combined
func (dc *DatasetsController) MarkUncombined(name string) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.uncombined[name] = true
}

// IsUncombined checks if a dataset needs combining
func (dc *DatasetsController) IsUncombined(name string) bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.uncombined[name]
}

// HasUncombined returns true if any datasets need combining
func (dc *DatasetsController) HasUncombined() bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return len(dc.uncombined) > 0
}

// ClearUncombined clears all uncombined markers
func (dc *DatasetsController) ClearUncombined() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.uncombined = make(map[string]bool)
}

// GetActiveDataset returns the currently active dataset name (empty if combined)
func (dc *DatasetsController) GetActiveDataset() string {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.activeDataset
}

// IsCombined returns true if multiple datasets are combined
func (dc *DatasetsController) IsCombined() bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.isCombined
}

// reloadTileserverIfConfigured calls the reload callback if set
func (dc *DatasetsController) reloadTileserverIfConfigured() {
	if dc.reloadTileserver != nil {
		if err := dc.reloadTileserver(); err != nil {
			slog.Error("Failed to reload tileserver", "error", err)
		} else {
			slog.Info("Tileserver reload triggered")
		}
	}
}

// SetActive sets a single dataset as the active/combined one (symlink)
func (dc *DatasetsController) SetActive(name string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid dataset name: %w", err)
	}

	sourcePath := filepath.Join(dc.listFolder, sanitized+".mbtiles")
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		return fmt.Errorf("dataset not found: %s", name)
	}

	combinedPath := filepath.Join(dc.folder, "Combined.mbtiles")
	os.Remove(combinedPath)

	source := filepath.Join("List", sanitized+".mbtiles")
	if err := os.Symlink(source, combinedPath); err != nil {
		return fmt.Errorf("failed to link mbtiles: %w", err)
	}

	// Update state
	dc.mu.Lock()
	delete(dc.uncombined, sanitized)
	dc.activeDataset = sanitized
	dc.isCombined = false
	dc.mu.Unlock()

	// Reload tileserver
	dc.reloadTileserverIfConfigured()

	return nil
}

// GetDatasets returns a list of available datasets
func (dc *DatasetsController) GetDatasets() ([]string, error) {
	entries, err := os.ReadDir(dc.listFolder)
	if err != nil {
		return nil, err
	}

	var datasets []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".mbtiles") {
			name := strings.TrimSuffix(entry.Name(), ".mbtiles")
			datasets = append(datasets, name)
		}
	}

	return datasets, nil
}

// AddDataset adds a new dataset from file data
func (dc *DatasetsController) AddDataset(name string, data []byte) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid dataset name: %w", err)
	}
	path := filepath.Join(dc.listFolder, sanitized+".mbtiles")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write dataset: %w", err)
	}

	return dc.CombineTiles()
}

// DeleteDataset removes a dataset
func (dc *DatasetsController) DeleteDataset(name string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid dataset name: %w", err)
	}
	
	path := filepath.Join(dc.listFolder, sanitized+".mbtiles")
	
	// Check if file exists before attempting deletion
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// File doesn't exist, but we should still clean up any orphaned entries
		slog.Warn("Dataset file not found, cleaning up orphaned entries", "name", sanitized, "path", path)
	} else {
		// Remove the physical file
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to delete dataset: %w", err)
		}
		slog.Info("Dataset file deleted", "name", sanitized, "path", path)
	}

	// Clean up state and metrics regardless of file existence
	dc.mu.Lock()
	// Remove from uncombined markers
	delete(dc.uncombined, sanitized)
	
	// Check if this was the active dataset
	wasActive := dc.activeDataset == sanitized
	dc.mu.Unlock()

	// Remove from metrics
	if GlobalMetrics != nil {
		GlobalMetrics.DeleteDatasetSize(sanitized)
		slog.Info("Dataset metrics cleaned up", "name", sanitized)
	}

	// If this was the active dataset, clean up Combined.mbtiles
	if wasActive {
		combinedPath := filepath.Join(dc.folder, "Combined.mbtiles")
		if err := os.Remove(combinedPath); err != nil && !os.IsNotExist(err) {
			slog.Error("Failed to remove Combined.mbtiles symlink", "error", err)
		} else {
			slog.Info("Removed Combined.mbtiles symlink for deleted active dataset", "name", sanitized)
		}
		
		// Update state
		dc.mu.Lock()
		dc.activeDataset = ""
		dc.isCombined = false
		dc.mu.Unlock()
		
		// Reload tileserver to reflect changes
		dc.reloadTileserverIfConfigured()
	}

	slog.Info("Dataset deletion completed", "name", sanitized)
	return nil
}

// GetListFolder returns the list folder path
func (dc *DatasetsController) GetListFolder() string {
	return dc.listFolder
}

// UpdateAllDatasetSizes updates the size metrics for all datasets
func (dc *DatasetsController) UpdateAllDatasetSizes() {
	if GlobalMetrics == nil {
		return
	}

	entries, err := os.ReadDir(dc.listFolder)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".mbtiles") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".mbtiles")
		GlobalMetrics.SetDatasetSize(name, uint64(info.Size()))
	}
}

// UpdateDatasetSize updates the size metric for a single dataset
func (dc *DatasetsController) UpdateDatasetSize(name string) {
	if GlobalMetrics == nil {
		return
	}

	path := filepath.Join(dc.listFolder, name+".mbtiles")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	GlobalMetrics.SetDatasetSize(name, uint64(info.Size()))
}

// CombineTiles combines all datasets into a single mbtiles file
func (dc *DatasetsController) CombineTiles() error {
	datasets, err := dc.GetDatasets()
	if err != nil {
		return fmt.Errorf("failed to get datasets: %w", err)
	}
	return dc.combineDatasets(datasets)
}

// CombineSelected combines specific datasets into a single mbtiles file
func (dc *DatasetsController) CombineSelected(datasets []string) error {
	// Validate all datasets exist
	for _, name := range datasets {
		sanitized, err := SanitizeName(name)
		if err != nil {
			return fmt.Errorf("invalid dataset name: %w", err)
		}
		sourcePath := filepath.Join(dc.listFolder, sanitized+".mbtiles")
		if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
			return fmt.Errorf("dataset not found: %s", name)
		}
	}
	return dc.combineDatasets(datasets)
}

// combineDatasets is the internal implementation for combining datasets
func (dc *DatasetsController) combineDatasets(datasets []string) error {
	combinedPath := filepath.Join(dc.folder, "Combined.mbtiles")

	if len(datasets) == 0 {
		os.Remove(combinedPath)
		dc.mu.Lock()
		dc.activeDataset = ""
		dc.isCombined = false
		dc.mu.Unlock()
		return nil
	}

	if len(datasets) == 1 {
		// Just symlink - auto-activate the single dataset
		os.Remove(combinedPath)
		source := filepath.Join("List", datasets[0]+".mbtiles")
		if err := os.Symlink(source, combinedPath); err != nil {
			return fmt.Errorf("failed to link mbtiles: %w", err)
		}
		dc.mu.Lock()
		dc.activeDataset = datasets[0]
		dc.isCombined = false
		dc.mu.Unlock()
		dc.reloadTileserverIfConfigured()
		return nil
	}

	// Use tile-join to combine multiple datasets
	args := []string{"--force", "-o", "Combined.mbtiles"}
	for _, ds := range datasets {
		args = append(args, filepath.Join("List", ds+".mbtiles"))
	}

	cmd := exec.Command(tileJoinCommand, args...)
	cmd.Dir = dc.folder
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Failed to combine mbtiles", "error", err, "output", string(output))
		return fmt.Errorf("failed to combine mbtiles: %s", string(output))
	}

	dc.mu.Lock()
	dc.activeDataset = ""
	dc.isCombined = true
	dc.mu.Unlock()

	// Update dataset sizes after combining
	dc.UpdateAllDatasetSizes()

	dc.reloadTileserverIfConfigured()

	return nil
}

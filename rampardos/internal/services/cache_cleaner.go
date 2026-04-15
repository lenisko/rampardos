package services

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Global registry of cache cleaners by folder name
var (
	cacheCleanerRegistry   = make(map[string]*CacheCleaner)
	cacheCleanerRegistryMu sync.RWMutex
)

// GetCacheCleaner returns a cache cleaner by folder name (e.g., "Tile", "Static")
func GetCacheCleaner(folderName string) *CacheCleaner {
	cacheCleanerRegistryMu.RLock()
	defer cacheCleanerRegistryMu.RUnlock()
	return cacheCleanerRegistry[folderName]
}

// GetCacheCleanerDelay returns the delay in seconds for a cache cleaner by folder name
func GetCacheCleanerDelay(folderName string) uint32 {
	cacheCleanerRegistryMu.RLock()
	defer cacheCleanerRegistryMu.RUnlock()
	if cc, ok := cacheCleanerRegistry[folderName]; ok {
		return cc.clearDelaySeconds
	}
	return 0
}

// CacheCleaner periodically removes old cached files
type CacheCleaner struct {
	folder            string
	maxAgeMinutes     uint32
	clearDelaySeconds uint32
	dropAfterMinutes  uint32
	dropAll           bool
	dropAllMu         sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *slog.Logger
}

// NewCacheCleaner creates a new cache cleaner
func NewCacheCleaner(folder string, maxAgeMinutes, clearDelaySeconds, dropAfterMinutes *uint32) *CacheCleaner {
	// Ensure folder exists
	os.MkdirAll(folder, 0755)

	ctx, cancel := context.WithCancel(context.Background())
	cc := &CacheCleaner{
		folder: folder,
		ctx:    ctx,
		cancel: cancel,
		logger: slog.With("component", "CacheCleaner", "folder", folder),
	}

	if maxAgeMinutes != nil {
		cc.maxAgeMinutes = *maxAgeMinutes
	}
	if clearDelaySeconds != nil {
		cc.clearDelaySeconds = *clearDelaySeconds
	}
	if dropAfterMinutes != nil {
		cc.dropAfterMinutes = *dropAfterMinutes
	}

	// Register in global registry
	folderName := filepath.Base(folder)
	cacheCleanerRegistryMu.Lock()
	cacheCleanerRegistry[folderName] = cc
	cacheCleanerRegistryMu.Unlock()

	return cc
}

// Start begins the background cleanup loop
func (cc *CacheCleaner) Start() {
	if cc.maxAgeMinutes == 0 || cc.clearDelaySeconds == 0 {
		return
	}

	cc.logger.Info("Starting CacheCleaner",
		"maxAgeMinutes", cc.maxAgeMinutes,
		"clearDelaySeconds", cc.clearDelaySeconds)

	go cc.run()
}

// Stop stops the cache cleaner
func (cc *CacheCleaner) Stop() {
	cc.cancel()
}

func (cc *CacheCleaner) run() {
	// Run immediately on start
	cc.runOnce()

	ticker := time.NewTicker(time.Duration(cc.clearDelaySeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cc.ctx.Done():
			return
		case <-ticker.C:
			cc.runOnce()
		}
	}
}

// ScheduleDropAll schedules removal of all files on next cleaner run
func (cc *CacheCleaner) ScheduleDropAll() {
	cc.dropAllMu.Lock()
	cc.dropAll = true
	cc.dropAllMu.Unlock()
	cc.logger.Info("Scheduled drop all files on next run")
}

// IsDropPending returns true if a drop all is scheduled
func (cc *CacheCleaner) IsDropPending() bool {
	cc.dropAllMu.Lock()
	defer cc.dropAllMu.Unlock()
	return cc.dropAll
}

// GetPendingDrops returns a list of folder names with pending drops
func GetPendingDrops() []string {
	cacheCleanerRegistryMu.RLock()
	defer cacheCleanerRegistryMu.RUnlock()

	var pending []string
	for name, cc := range cacheCleanerRegistry {
		if cc.IsDropPending() {
			pending = append(pending, name)
		}
	}
	return pending
}

func (cc *CacheCleaner) runOnce() {
	// Check if drop all is scheduled
	cc.dropAllMu.Lock()
	dropAll := cc.dropAll
	cc.dropAllMu.Unlock()

	cutoff := time.Now().Add(-time.Duration(cc.maxAgeMinutes) * time.Minute)
	var dropCutoff time.Time
	if cc.dropAfterMinutes > 0 {
		dropCutoff = time.Now().Add(-time.Duration(cc.dropAfterMinutes) * time.Minute)
	}
	count := 0

	entries, err := os.ReadDir(cc.folder)
	if err != nil {
		cc.logger.Warn("Failed to read directory", "error", err)
		return
	}

	// First pass: count files to be removed (queue size)
	toRemove := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if dropAll {
			toRemove++
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if cc.shouldRemove(info, cutoff, dropCutoff) {
			toRemove++
		}
	}

	// Report queue size to metrics
	if GlobalMetrics != nil {
		GlobalMetrics.SetFileRemoverQueueSize(cc.folder, toRemove)
	}

	// Second pass: remove files
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(cc.folder, entry.Name())

		if dropAll {
			if err := os.Remove(path); err == nil {
				count++
			}
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if cc.shouldRemove(info, cutoff, dropCutoff) {
			if err := os.Remove(path); err == nil {
				count++
			}
		}
	}

	if dropAll {
		cc.dropAllMu.Lock()
		cc.dropAll = false
		cc.dropAllMu.Unlock()
		cc.logger.Info("Drop all completed", "count", count)
	} else {
		cc.logger.Info("Removed files", "count", count)
	}

	// Calculate and report cache size
	cc.updateCacheSize()
}

// updateCacheSize calculates the total size of the cache folder and reports it
func (cc *CacheCleaner) updateCacheSize() {
	if GlobalMetrics == nil {
		return
	}

	var totalSize uint64
	entries, err := os.ReadDir(cc.folder)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		totalSize += uint64(info.Size())
	}

	// Use just the folder name for the label (e.g., "Tile" from "Cache/Tile")
	folderName := filepath.Base(cc.folder)
	GlobalMetrics.SetCacheSize(folderName, totalSize)
}

// shouldRemove checks if a file should be removed based on mod time (maxAge) or creation time (dropAfter)
func (cc *CacheCleaner) shouldRemove(info os.FileInfo, cutoff, dropCutoff time.Time) bool {
	// Remove if not accessed/modified within maxAge
	if info.ModTime().Before(cutoff) {
		return true
	}
	// Remove if created before dropCutoff (force refresh)
	if !dropCutoff.IsZero() {
		birthTime := getFileCreationTime(info)
		if !birthTime.IsZero() && birthTime.Before(dropCutoff) {
			return true
		}
	}
	return false
}


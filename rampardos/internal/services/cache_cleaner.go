package services

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CacheCleaner periodically removes old cached files
type CacheCleaner struct {
	folder            string
	maxAgeMinutes     uint32
	clearDelaySeconds uint32
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *slog.Logger
}

// NewCacheCleaner creates a new cache cleaner
func NewCacheCleaner(folder string, maxAgeMinutes, clearDelaySeconds *uint32) *CacheCleaner {
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

func (cc *CacheCleaner) runOnce() {
	cutoff := time.Now().Add(-time.Duration(cc.maxAgeMinutes) * time.Minute)
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
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
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
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				count++
				// Remove from cache index
				if GlobalCacheIndex != nil {
					cc.removeFromCacheIndex(path)
				}
			}
		}
	}

	cc.logger.Info("Removed files", "count", count)
}

// removeFromCacheIndex removes a path from the appropriate cache index based on folder
func (cc *CacheCleaner) removeFromCacheIndex(path string) {
	switch {
	case strings.HasPrefix(cc.folder, "Cache/Static"):
		GlobalCacheIndex.RemoveStaticMap(path)
	case strings.HasPrefix(cc.folder, "Cache/StaticMulti"):
		GlobalCacheIndex.RemoveMultiStaticMap(path)
	case strings.HasPrefix(cc.folder, "Cache/Marker"):
		GlobalCacheIndex.RemoveMarker(path)
	case strings.HasPrefix(cc.folder, "Cache/Tile"):
		GlobalCacheIndex.RemoveTile(path)
	}
}

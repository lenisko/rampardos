package services

import (
	"sync"
)

// CacheIndex maintains an in-memory index of cached files to reduce syscalls
type CacheIndex struct {
	staticMaps      map[string]struct{}
	multiStaticMaps map[string]struct{}
	markers         map[string]struct{}
	tiles           map[string]struct{}
	mu              sync.RWMutex
}

// GlobalCacheIndex is the global cache index instance
var GlobalCacheIndex *CacheIndex

// NewCacheIndex creates a new cache index
func NewCacheIndex() *CacheIndex {
	return &CacheIndex{
		staticMaps:      make(map[string]struct{}),
		multiStaticMaps: make(map[string]struct{}),
		markers:         make(map[string]struct{}),
		tiles:           make(map[string]struct{}),
	}
}

// InitGlobalCacheIndex initializes the global cache index
func InitGlobalCacheIndex() {
	GlobalCacheIndex = NewCacheIndex()
}

// HasStaticMap checks if a static map is in the cache index
func (c *CacheIndex) HasStaticMap(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.staticMaps[path]
	return ok
}

// AddStaticMap adds a static map to the cache index
func (c *CacheIndex) AddStaticMap(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.staticMaps[path] = struct{}{}
}

// RemoveStaticMap removes a static map from the cache index
func (c *CacheIndex) RemoveStaticMap(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.staticMaps, path)
}

// HasMultiStaticMap checks if a multi static map is in the cache index
func (c *CacheIndex) HasMultiStaticMap(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.multiStaticMaps[path]
	return ok
}

// AddMultiStaticMap adds a multi static map to the cache index
func (c *CacheIndex) AddMultiStaticMap(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.multiStaticMaps[path] = struct{}{}
}

// RemoveMultiStaticMap removes a multi static map from the cache index
func (c *CacheIndex) RemoveMultiStaticMap(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.multiStaticMaps, path)
}

// HasMarker checks if a marker is in the cache index
func (c *CacheIndex) HasMarker(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.markers[path]
	return ok
}

// AddMarker adds a marker to the cache index
func (c *CacheIndex) AddMarker(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markers[path] = struct{}{}
}

// RemoveMarker removes a marker from the cache index
func (c *CacheIndex) RemoveMarker(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.markers, path)
}

// HasTile checks if a tile is in the cache index
func (c *CacheIndex) HasTile(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.tiles[path]
	return ok
}

// AddTile adds a tile to the cache index
func (c *CacheIndex) AddTile(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tiles[path] = struct{}{}
}

// RemoveTile removes a tile from the cache index
func (c *CacheIndex) RemoveTile(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tiles, path)
}

// Stats returns cache index statistics
func (c *CacheIndex) Stats() (staticMaps, multiStaticMaps, markers, tiles int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.staticMaps), len(c.multiStaticMaps), len(c.markers), len(c.tiles)
}

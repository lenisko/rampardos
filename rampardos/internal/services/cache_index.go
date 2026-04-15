package services

import (
	"container/list"
	"fmt"
	"image"
	"sync"
)

// markerImageEntry holds a cached resized marker image
type markerImageEntry struct {
	key   string
	image image.Image
}

// CacheIndex maintains an in-memory index of cached files to reduce
// syscalls on the serve path. Only the final staticmap/multistaticmap
// paths are indexed — tile and marker caches are managed entirely via
// the filesystem and CacheCleaner, since in-memory presence checks
// gave no measurable benefit for those paths.
type CacheIndex struct {
	staticMaps      map[string]struct{}
	multiStaticMaps map[string]struct{}
	mu              sync.RWMutex

	// LRU cache for resized marker images (path+size -> image.Image).
	// Unlike the file-presence indices, this saves real work — decode
	// plus bicubic resize is millisecond-scale per marker.
	markerImages    map[string]*list.Element
	markerImagesLRU *list.List
	markerImagesMax int
	markerImagesMu  sync.RWMutex
}

// GlobalCacheIndex is the global cache index instance
var GlobalCacheIndex *CacheIndex

// NewCacheIndex creates a new cache index
func NewCacheIndex() *CacheIndex {
	return &CacheIndex{
		staticMaps:      make(map[string]struct{}),
		multiStaticMaps: make(map[string]struct{}),
		markerImages:    make(map[string]*list.Element),
		markerImagesLRU: list.New(),
		markerImagesMax: 500, // Default, can be changed via SetMarkerImageCacheSize
	}
}

// SetMarkerImageCacheSize sets the maximum number of resized marker images to cache
func (c *CacheIndex) SetMarkerImageCacheSize(size int) {
	c.markerImagesMu.Lock()
	defer c.markerImagesMu.Unlock()
	c.markerImagesMax = size
	// Evict if over new limit
	for c.markerImagesLRU.Len() > c.markerImagesMax {
		c.evictOldestMarkerImageLocked()
	}
}

// GetMarkerImage retrieves a cached resized marker image by path and target size
func (c *CacheIndex) GetMarkerImage(path string, width, height int) (image.Image, bool) {
	key := markerImageKey(path, width, height)
	c.markerImagesMu.Lock()
	defer c.markerImagesMu.Unlock()

	if elem, ok := c.markerImages[key]; ok {
		// Move to front (most recently used)
		c.markerImagesLRU.MoveToFront(elem)
		return elem.Value.(*markerImageEntry).image, true
	}
	return nil, false
}

// AddMarkerImage adds a resized marker image to the LRU cache
func (c *CacheIndex) AddMarkerImage(path string, width, height int, img image.Image) {
	key := markerImageKey(path, width, height)
	c.markerImagesMu.Lock()
	defer c.markerImagesMu.Unlock()

	// Already exists? Move to front
	if elem, ok := c.markerImages[key]; ok {
		c.markerImagesLRU.MoveToFront(elem)
		return
	}

	// Evict oldest if at capacity
	if c.markerImagesLRU.Len() >= c.markerImagesMax {
		c.evictOldestMarkerImageLocked()
	}

	// Add new entry
	entry := &markerImageEntry{key: key, image: img}
	elem := c.markerImagesLRU.PushFront(entry)
	c.markerImages[key] = elem
}

func (c *CacheIndex) evictOldestMarkerImageLocked() {
	oldest := c.markerImagesLRU.Back()
	if oldest != nil {
		c.markerImagesLRU.Remove(oldest)
		delete(c.markerImages, oldest.Value.(*markerImageEntry).key)
	}
}

func markerImageKey(path string, width, height int) string {
	return fmt.Sprintf("%s@%dx%d", path, width, height)
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


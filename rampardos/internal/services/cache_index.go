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

// CacheIndex holds an LRU cache of decoded+resized marker images. It
// no longer tracks static-map / multi-static-map / marker file presence
// — those "caches" just saved a sub-microsecond stat syscall and
// introduced a latent failure mode when index entries outlived the
// on-disk files (stale-lie 404 / silently-missing marker). The LRU
// below, in contrast, saves real work (image decode plus bicubic
// resize, millisecond-scale per marker).
type CacheIndex struct {
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
	for c.markerImagesLRU.Len() > c.markerImagesMax {
		c.evictOldestMarkerImageLocked()
	}
}

// GetMarkerImage retrieves a cached resized marker image by path and target size.
// Records hit/miss to GlobalMetrics when set.
func (c *CacheIndex) GetMarkerImage(path string, width, height int) (image.Image, bool) {
	key := markerImageKey(path, width, height)
	c.markerImagesMu.Lock()

	if elem, ok := c.markerImages[key]; ok {
		c.markerImagesLRU.MoveToFront(elem)
		img := elem.Value.(*markerImageEntry).image
		c.markerImagesMu.Unlock()
		if GlobalMetrics != nil {
			GlobalMetrics.RecordImageCacheHit(ImageCacheMarker)
		}
		return img, true
	}
	c.markerImagesMu.Unlock()
	if GlobalMetrics != nil {
		GlobalMetrics.RecordImageCacheMiss(ImageCacheMarker)
	}
	return nil, false
}

// AddMarkerImage adds a resized marker image to the LRU cache
func (c *CacheIndex) AddMarkerImage(path string, width, height int, img image.Image) {
	key := markerImageKey(path, width, height)
	c.markerImagesMu.Lock()
	defer c.markerImagesMu.Unlock()

	if elem, ok := c.markerImages[key]; ok {
		c.markerImagesLRU.MoveToFront(elem)
		return
	}

	if c.markerImagesLRU.Len() >= c.markerImagesMax {
		c.evictOldestMarkerImageLocked()
	}

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

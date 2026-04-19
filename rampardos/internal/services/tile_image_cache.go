package services

import (
	"container/list"
	"image"
	"sync"
)

// TileImageCache is an LRU of decoded tile images, keyed by cache
// path. Purpose is to skip the PNG decode on every base-map stitch —
// pprof showed image.Decode dominating 54% of CPU on the static-map
// hot path despite 99.9% disk-cache hits, because each hit re-opened
// the file and re-decoded it.
//
// Entries are stored as whatever image.Image the caller provides (in
// practice *image.NRGBA from png.Decode) so callers can memcpy the
// result straight into their stitch canvas without format
// conversion.
//
// A size of 0 disables the cache — Add is a no-op and Get always
// misses, so operators can switch it off via env without wiring
// conditional code through call sites.
type TileImageCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	maxSize int
}

type tileImageEntry struct {
	key string
	img image.Image
}

// NewTileImageCache creates a TileImageCache with the given capacity.
// A size of 0 makes the cache a no-op.
func NewTileImageCache(size int) *TileImageCache {
	return &TileImageCache{
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		maxSize: size,
	}
}

// Get returns the cached image for key, or (nil, false) on miss.
// Touches recency on hit. Records hit/miss to GlobalMetrics when set.
func (c *TileImageCache) Get(key string) (image.Image, bool) {
	c.mu.Lock()
	if c.maxSize <= 0 {
		c.mu.Unlock()
		// A disabled cache is neither a hit nor a miss — don't pollute
		// the ratio. Callers that care can instrument separately.
		return nil, false
	}
	elem, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		if GlobalMetrics != nil {
			GlobalMetrics.RecordImageCacheMiss("tile")
		}
		return nil, false
	}
	c.lru.MoveToFront(elem)
	img := elem.Value.(*tileImageEntry).img
	c.mu.Unlock()
	if GlobalMetrics != nil {
		GlobalMetrics.RecordImageCacheHit("tile")
	}
	return img, true
}

// Add inserts or refreshes key → img. Evicts the oldest entry when
// capacity is reached. No-op when the cache is disabled (size 0).
func (c *TileImageCache) Add(key string, img image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxSize <= 0 {
		return
	}
	if elem, ok := c.entries[key]; ok {
		elem.Value.(*tileImageEntry).img = img
		c.lru.MoveToFront(elem)
		return
	}
	for c.lru.Len() >= c.maxSize {
		c.evictOldestLocked()
	}
	entry := &tileImageEntry{key: key, img: img}
	elem := c.lru.PushFront(entry)
	c.entries[key] = elem
}

// SetSize changes the cache capacity, evicting the oldest entries
// until the length fits. SetSize(0) empties and disables the cache.
func (c *TileImageCache) SetSize(size int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maxSize = size
	for c.lru.Len() > c.maxSize {
		c.evictOldestLocked()
	}
}

func (c *TileImageCache) evictOldestLocked() {
	oldest := c.lru.Back()
	if oldest == nil {
		return
	}
	c.lru.Remove(oldest)
	delete(c.entries, oldest.Value.(*tileImageEntry).key)
}

// GlobalTileImageCache is the process-wide tile image cache. Nil
// before InitGlobalTileImageCache runs; callers must nil-check.
var GlobalTileImageCache *TileImageCache

// InitGlobalTileImageCache installs the global tile image cache with
// the given capacity.
func InitGlobalTileImageCache(size int) {
	GlobalTileImageCache = NewTileImageCache(size)
}

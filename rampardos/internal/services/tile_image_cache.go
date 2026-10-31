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
// The cache is bounded by decoded pixel bytes as well as entry count:
// a 256 px tile is 256 KB but a 512 px or @2x external tile is 1 MB,
// so a count sized for the small case can hold four times the memory
// it was meant to.
//
// A size or byte budget of 0 disables the cache — Add is a no-op and
// Get always misses, so operators can switch it off via env without
// wiring conditional code through call sites.
type TileImageCache struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	lru      *list.List
	maxSize  int
	maxBytes int64
	bytes    int64
}

type tileImageEntry struct {
	key   string
	img   image.Image
	bytes int64
}

func NewTileImageCache(size int, maxBytes int64) *TileImageCache {
	return &TileImageCache{
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
		maxSize:  size,
		maxBytes: maxBytes,
	}
}

func (c *TileImageCache) disabledLocked() bool {
	return c.maxSize <= 0 || c.maxBytes <= 0
}

func (c *TileImageCache) Get(key string) (image.Image, bool) {
	c.mu.Lock()
	if c.disabledLocked() {
		c.mu.Unlock()
		// A disabled cache is neither a hit nor a miss — don't pollute
		// the ratio. Callers that care can instrument separately.
		return nil, false
	}
	elem, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		if GlobalMetrics != nil {
			GlobalMetrics.RecordImageCacheMiss(ImageCacheTile)
		}
		return nil, false
	}
	c.lru.MoveToFront(elem)
	img := elem.Value.(*tileImageEntry).img
	c.mu.Unlock()
	if GlobalMetrics != nil {
		GlobalMetrics.RecordImageCacheHit(ImageCacheTile)
	}
	return img, true
}

func (c *TileImageCache) Add(key string, img image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.disabledLocked() || img == nil {
		return
	}
	size := compositeImageBytes(img)
	if elem, ok := c.entries[key]; ok {
		c.removeLocked(elem)
	}
	if size > c.maxBytes {
		c.publishLocked()
		return
	}
	entry := &tileImageEntry{key: key, img: img, bytes: size}
	c.entries[key] = c.lru.PushFront(entry)
	c.bytes += size
	c.trimLocked()
	c.publishLocked()
}

func (c *TileImageCache) SetSize(size int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maxSize = size
	c.trimLocked()
	c.publishLocked()
}

// Bytes returns the decoded pixel bytes currently held.
func (c *TileImageCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// trimLocked evicts from the LRU tail until both limits hold.
func (c *TileImageCache) trimLocked() {
	for c.lru.Len() > 0 && (c.lru.Len() > c.maxSize || c.bytes > c.maxBytes) {
		c.removeLocked(c.lru.Back())
		if GlobalMetrics != nil {
			GlobalMetrics.RecordImageCacheEviction(ImageCacheTile)
		}
	}
}

func (c *TileImageCache) removeLocked(elem *list.Element) {
	entry := elem.Value.(*tileImageEntry)
	c.lru.Remove(elem)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
}

// publishLocked updates the byte gauge under the lock so concurrent
// updates cannot publish out of order.
func (c *TileImageCache) publishLocked() {
	if GlobalMetrics != nil {
		GlobalMetrics.SetImageCacheBytes(ImageCacheTile, c.bytes)
	}
}

// GlobalTileImageCache is the process-wide tile image cache. Nil
// before InitGlobalTileImageCache runs; callers must nil-check.
var GlobalTileImageCache *TileImageCache

func InitGlobalTileImageCache(size int, maxBytes int64) {
	GlobalTileImageCache = NewTileImageCache(size, maxBytes)
}

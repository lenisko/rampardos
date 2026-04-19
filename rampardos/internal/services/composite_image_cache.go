package services

import (
	"container/list"
	"image"
	"sync"
)

// CompositeImageCache is an LRU of staticmap / basePath images. The
// tile LRU handles 256×256 tile reuse; this one holds larger
// composed images (base renders, final staticmap outputs) keyed by
// the path string that would otherwise have been the on-disk
// filename. Within a single request producer and consumer pass
// images directly; this LRU is the cross-request bridge that
// replaces the short-TTL disk cache for burst sharing.
//
// Size 0 disables the cache (Add is a no-op, Get always misses).
type CompositeImageCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	maxSize int
}

type compositeImageEntry struct {
	key string
	img image.Image
}

func NewCompositeImageCache(size int) *CompositeImageCache {
	return &CompositeImageCache{
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		maxSize: size,
	}
}

func (c *CompositeImageCache) Get(key string) (image.Image, bool) {
	c.mu.Lock()
	if c.maxSize <= 0 {
		c.mu.Unlock()
		return nil, false
	}
	elem, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		if GlobalMetrics != nil {
			GlobalMetrics.RecordImageCacheMiss(ImageCacheComposite)
		}
		return nil, false
	}
	c.lru.MoveToFront(elem)
	img := elem.Value.(*compositeImageEntry).img
	c.mu.Unlock()
	if GlobalMetrics != nil {
		GlobalMetrics.RecordImageCacheHit(ImageCacheComposite)
	}
	return img, true
}

func (c *CompositeImageCache) Add(key string, img image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxSize <= 0 {
		return
	}
	if elem, ok := c.entries[key]; ok {
		elem.Value.(*compositeImageEntry).img = img
		c.lru.MoveToFront(elem)
		return
	}
	for c.lru.Len() >= c.maxSize {
		c.evictOldestLocked()
	}
	entry := &compositeImageEntry{key: key, img: img}
	elem := c.lru.PushFront(entry)
	c.entries[key] = elem
}

func (c *CompositeImageCache) SetSize(size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxSize = size
	for c.lru.Len() > c.maxSize {
		c.evictOldestLocked()
	}
}

func (c *CompositeImageCache) evictOldestLocked() {
	oldest := c.lru.Back()
	if oldest == nil {
		return
	}
	c.lru.Remove(oldest)
	delete(c.entries, oldest.Value.(*compositeImageEntry).key)
}

var GlobalCompositeImageCache *CompositeImageCache

func InitGlobalCompositeImageCache(size int) {
	GlobalCompositeImageCache = NewCompositeImageCache(size)
}

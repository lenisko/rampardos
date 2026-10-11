package services

import (
	"container/list"
	"image"
	"sync"
)

// Composite cache entry kinds. Kept as constants (like the metric
// label values in metrics.go) so a typo cannot split a label.
const (
	CompositeKindBase  = "base"  // rendered base map, shared by every request with the same viewport
	CompositeKindFinal = "final" // staticmap with drawables, shared by identical requests
	CompositeKindMulti = "multi" // composed multistaticmap grid
)

// CompositeImageCache is an LRU of staticmap / basePath images. The
// tile LRU handles 256×256 tile reuse; this one holds larger
// composed images (base renders, final staticmap outputs) keyed by
// the path string that would otherwise have been the on-disk
// filename. Within a single request producer and consumer pass
// images directly; this LRU is the cross-request bridge that
// replaces the short-TTL disk cache for burst sharing.
//
// The cache is bounded by the bytes of decoded pixels it holds, not
// by entry count: a scale=2 multistaticmap is tens of times larger
// than a scale=1 staticmap, so a count limit sized for the small case
// let the cache reach ~1 GB in prod. maxEntries is a secondary cap.
//
// maxEntries or maxBytes of 0 disables the cache (Add is a no-op,
// Get always misses).
type CompositeImageCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        *list.List
	maxEntries int
	maxBytes   int64
	bytes      int64
	kindBytes  map[string]int64
	evictions  uint64
}

type compositeImageEntry struct {
	key   string
	kind  string
	img   image.Image
	bytes int64
}

func NewCompositeImageCache(maxEntries int, maxBytes int64) *CompositeImageCache {
	return &CompositeImageCache{
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		kindBytes:  make(map[string]int64),
	}
}

// compositeImageBytes is the decoded pixel footprint of img: the
// pixel buffer for the types the pipeline produces, else w*h*4.
func compositeImageBytes(img image.Image) int64 {
	switch m := img.(type) {
	case *image.NRGBA:
		return int64(len(m.Pix))
	case *image.RGBA:
		return int64(len(m.Pix))
	}
	b := img.Bounds()
	return int64(b.Dx()) * int64(b.Dy()) * 4
}

func (c *CompositeImageCache) disabledLocked() bool {
	return c.maxEntries <= 0 || c.maxBytes <= 0
}

// Get looks key up and records a hit or miss. Call it once per
// logical lookup; a double-checked lookup inside singleflight uses
// Recheck so one request is not counted twice.
func (c *CompositeImageCache) Get(kind, key string) (image.Image, bool) {
	c.mu.Lock()
	if c.disabledLocked() {
		c.mu.Unlock()
		return nil, false
	}
	img, ok := c.lookupLocked(key)
	c.mu.Unlock()
	if GlobalMetrics != nil {
		GlobalMetrics.RecordCompositeCacheLookup(kind, ok)
	}
	return img, ok
}

// Recheck is Get without metrics, for the second lookup of a
// double-checked pattern whose first Get already counted the request.
// A hit still promotes the entry: the image is being served.
func (c *CompositeImageCache) Recheck(key string) (image.Image, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabledLocked() {
		return nil, false
	}
	return c.lookupLocked(key)
}

func (c *CompositeImageCache) lookupLocked(key string) (image.Image, bool) {
	elem, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(elem)
	return elem.Value.(*compositeImageEntry).img, true
}

// Add caches img under key. An image larger than the whole byte
// budget is not cached (and drops any stale entry for key) rather
// than flushing everything else to make room.
func (c *CompositeImageCache) Add(kind, key string, img image.Image) {
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
	entry := &compositeImageEntry{key: key, kind: kind, img: img, bytes: size}
	c.entries[key] = c.lru.PushFront(entry)
	c.bytes += size
	c.kindBytes[kind] += size
	c.trimLocked()
	c.publishLocked()
}

// SetSize sets the secondary entry-count cap.
func (c *CompositeImageCache) SetSize(maxEntries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxEntries = maxEntries
	c.trimLocked()
	c.publishLocked()
}

// SetMaxBytes sets the byte budget.
func (c *CompositeImageCache) SetMaxBytes(maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = maxBytes
	c.trimLocked()
	c.publishLocked()
}

// Bytes returns the decoded pixel bytes currently held.
func (c *CompositeImageCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Evictions returns how many entries the limits have pushed out.
func (c *CompositeImageCache) Evictions() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictions
}

// trimLocked evicts from the LRU tail until both limits hold.
func (c *CompositeImageCache) trimLocked() {
	for c.lru.Len() > 0 && (c.lru.Len() > c.maxEntries || c.bytes > c.maxBytes) {
		c.removeLocked(c.lru.Back())
		c.evictions++
		if GlobalMetrics != nil {
			GlobalMetrics.RecordImageCacheEviction(ImageCacheComposite)
		}
	}
}

func (c *CompositeImageCache) removeLocked(elem *list.Element) {
	entry := elem.Value.(*compositeImageEntry)
	c.lru.Remove(elem)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
	c.kindBytes[entry.kind] -= entry.bytes
}

// publishLocked updates the byte gauges. Called under the lock so
// concurrent updates cannot publish out of order. Every kind is
// published because any limit-driven eviction can shrink any kind.
func (c *CompositeImageCache) publishLocked() {
	if GlobalMetrics == nil {
		return
	}
	GlobalMetrics.SetImageCacheBytes(ImageCacheComposite, c.bytes)
	for _, k := range []string{CompositeKindBase, CompositeKindFinal, CompositeKindMulti} {
		GlobalMetrics.SetCompositeCacheKindBytes(k, c.kindBytes[k])
	}
}

var GlobalCompositeImageCache *CompositeImageCache

func InitGlobalCompositeImageCache(maxEntries int, maxBytes int64) {
	GlobalCompositeImageCache = NewCompositeImageCache(maxEntries, maxBytes)
}

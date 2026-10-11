package services

import (
	"image"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func makeCompositeTestImage(w, h int) image.Image {
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}

func TestCompositeImageCacheRoundTrip(t *testing.T) {
	c := NewCompositeImageCache(4, 1<<30)
	img := makeCompositeTestImage(2, 2)
	c.Add(CompositeKindFinal, "a", img)

	got, ok := c.Get(CompositeKindFinal, "a")
	if !ok {
		t.Fatal("expected hit on freshly-added key")
	}
	if got != img {
		t.Fatal("expected the same image pointer back")
	}
}

func TestCompositeImageCacheMiss(t *testing.T) {
	c := NewCompositeImageCache(4, 1<<30)
	if _, ok := c.Get(CompositeKindFinal, "missing"); ok {
		t.Fatal("expected miss on unknown key")
	}
}

func TestCompositeImageCacheEvictsOldest(t *testing.T) {
	c := NewCompositeImageCache(3, 1<<30)
	c.Add(CompositeKindFinal, "a", makeCompositeTestImage(1, 1))
	c.Add(CompositeKindFinal, "b", makeCompositeTestImage(1, 1))
	c.Add(CompositeKindFinal, "c", makeCompositeTestImage(1, 1))

	if _, ok := c.Get(CompositeKindFinal, "a"); !ok {
		t.Fatal("a should still be cached")
	}
	c.Add(CompositeKindFinal, "d", makeCompositeTestImage(1, 1))

	if _, ok := c.Get(CompositeKindFinal, "b"); ok {
		t.Fatal("b should have been evicted as least-recently-used")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(CompositeKindFinal, k); !ok {
			t.Fatalf("%s should still be cached", k)
		}
	}
}

func TestCompositeImageCacheReAddMovesToFront(t *testing.T) {
	c := NewCompositeImageCache(2, 1<<30)
	c.Add(CompositeKindFinal, "a", makeCompositeTestImage(1, 1))
	c.Add(CompositeKindFinal, "b", makeCompositeTestImage(1, 1))
	c.Add(CompositeKindFinal, "a", makeCompositeTestImage(1, 1))
	c.Add(CompositeKindFinal, "c", makeCompositeTestImage(1, 1))

	if _, ok := c.Get(CompositeKindFinal, "b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get(CompositeKindFinal, "a"); !ok {
		t.Fatal("a should survive re-add")
	}
}

func TestCompositeImageCacheSetSizeShrinks(t *testing.T) {
	c := NewCompositeImageCache(5, 1<<30)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		c.Add(CompositeKindFinal, k, makeCompositeTestImage(1, 1))
	}
	c.SetSize(2)
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := c.Get(CompositeKindFinal, k); ok {
			t.Fatalf("%s should have been evicted on shrink", k)
		}
	}
	for _, k := range []string{"d", "e"} {
		if _, ok := c.Get(CompositeKindFinal, k); !ok {
			t.Fatalf("%s should have survived shrink", k)
		}
	}
}

func TestCompositeImageCacheDisabledWhenSizeZero(t *testing.T) {
	c := NewCompositeImageCache(0, 1<<30)
	c.Add(CompositeKindFinal, "a", makeCompositeTestImage(1, 1))
	if _, ok := c.Get(CompositeKindFinal, "a"); ok {
		t.Fatal("zero-size cache should never hit")
	}
}

// nrgba returns an NRGBA image whose pixel buffer is w*h*4 bytes.
func nrgba(w, h int) image.Image { return image.NewNRGBA(image.Rect(0, 0, w, h)) }

func TestCompositeImageBytes(t *testing.T) {
	cases := []struct {
		name string
		img  image.Image
		want int64
	}{
		{"NRGBA", image.NewNRGBA(image.Rect(0, 0, 10, 20)), 800},
		{"RGBA", image.NewRGBA(image.Rect(0, 0, 10, 20)), 800},
		{"other falls back to w*h*4", image.NewGray(image.Rect(0, 0, 10, 20)), 800},
	}
	for _, tc := range cases {
		if got := compositeImageBytes(tc.img); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCompositeImageCacheTracksBytes(t *testing.T) {
	c := NewCompositeImageCache(100, 1<<20)
	c.Add(CompositeKindBase, "a", nrgba(10, 10)) // 400
	c.Add(CompositeKindFinal, "b", nrgba(5, 5))  // 100
	if got := c.Bytes(); got != 500 {
		t.Fatalf("bytes after two adds: got %d, want 500", got)
	}
	// Replacing a key re-accounts it rather than double counting.
	c.Add(CompositeKindBase, "a", nrgba(20, 10)) // 800
	if got := c.Bytes(); got != 900 {
		t.Fatalf("bytes after replace: got %d, want 900", got)
	}
}

func TestCompositeImageCacheEvictsTailUnderByteBudget(t *testing.T) {
	c := NewCompositeImageCache(100, 1000)
	c.Add(CompositeKindBase, "a", nrgba(10, 10)) // 400
	c.Add(CompositeKindBase, "b", nrgba(10, 10)) // 400
	c.Get(CompositeKindBase, "a")                // a is now most recent
	c.Add(CompositeKindBase, "c", nrgba(10, 10)) // 400 → 1200 > 1000, evict b

	if cached(c, "b") {
		t.Fatal("b should have been evicted as least recently used")
	}
	for _, k := range []string{"a", "c"} {
		if !cached(c, k) {
			t.Fatalf("%s should still be cached", k)
		}
	}
	if got := c.Bytes(); got != 800 {
		t.Fatalf("bytes: got %d, want 800", got)
	}
	if got := c.Evictions(); got != 1 {
		t.Fatalf("evictions: got %d, want 1", got)
	}
}

func TestCompositeImageCacheRejectsOversizeEntry(t *testing.T) {
	c := NewCompositeImageCache(100, 1000)
	c.Add(CompositeKindBase, "a", nrgba(10, 10))     // 400
	c.Add(CompositeKindMulti, "huge", nrgba(20, 20)) // 1600 > whole budget

	if cached(c, "huge") {
		t.Fatal("entry larger than the whole budget must not be cached")
	}
	if !cached(c, "a") {
		t.Fatal("rejecting an oversize entry must not flush the cache")
	}
	if got := c.Bytes(); got != 400 {
		t.Fatalf("bytes: got %d, want 400", got)
	}
}

func TestCompositeImageCacheOversizeReplaceDropsStaleEntry(t *testing.T) {
	c := NewCompositeImageCache(100, 1000)
	c.Add(CompositeKindFinal, "a", nrgba(10, 10))
	c.Add(CompositeKindFinal, "a", nrgba(20, 20)) // oversize replacement
	if cached(c, "a") {
		t.Fatal("an oversize replacement must not leave the stale image cached")
	}
	if got := c.Bytes(); got != 0 {
		t.Fatalf("bytes: got %d, want 0", got)
	}
}

func TestCompositeImageCacheSetMaxBytesShrinks(t *testing.T) {
	c := NewCompositeImageCache(100, 1<<20)
	for _, k := range []string{"a", "b", "c", "d"} {
		c.Add(CompositeKindBase, k, nrgba(10, 10)) // 400 each
	}
	c.SetMaxBytes(800)
	for _, k := range []string{"a", "b"} {
		if cached(c, k) {
			t.Fatalf("%s should have been evicted on budget shrink", k)
		}
	}
	for _, k := range []string{"c", "d"} {
		if !cached(c, k) {
			t.Fatalf("%s should have survived budget shrink", k)
		}
	}
	if got := c.Bytes(); got != 800 {
		t.Fatalf("bytes: got %d, want 800", got)
	}
}

func TestCompositeImageCacheDisabledWhenBudgetZero(t *testing.T) {
	c := NewCompositeImageCache(100, 0)
	c.Add(CompositeKindBase, "a", nrgba(1, 1))
	if cached(c, "a") {
		t.Fatal("zero byte budget should disable the cache")
	}
}

func TestCompositeImageCacheRecheckPromotes(t *testing.T) {
	c := NewCompositeImageCache(2, 1<<20)
	c.Add(CompositeKindBase, "a", nrgba(1, 1))
	c.Add(CompositeKindBase, "b", nrgba(1, 1))
	if _, ok := c.Recheck("a"); !ok { // served from cache: a real use
		t.Fatal("Recheck should hit")
	}
	c.Add(CompositeKindBase, "c", nrgba(1, 1))
	if !cached(c, "a") {
		t.Fatal("Recheck should move a to the front")
	}
	if cached(c, "b") {
		t.Fatal("b should have been evicted")
	}
}

// cached reports whether key is present without touching LRU order
// or metrics.
func cached(c *CompositeImageCache, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	return ok
}

func TestCompositeImageCacheMetrics(t *testing.T) {
	m := InitMetrics()
	lookups := func(kind, result string) float64 {
		return metricValue(t, m.compositeLookups.WithLabelValues(kind, result))
	}
	missBefore := lookups(CompositeKindBase, "miss")
	hitBefore := lookups(CompositeKindBase, "hit")
	evictBefore := metricValue(t, m.imageCacheEvictions.WithLabelValues(ImageCacheComposite))

	c := NewCompositeImageCache(100, 1000)
	c.Get(CompositeKindBase, "a") // counted miss
	c.Recheck("a")                // double-check: not counted
	c.Add(CompositeKindBase, "a", nrgba(10, 10))
	c.Get(CompositeKindBase, "a") // counted hit
	c.Recheck("a")                // not counted
	c.Add(CompositeKindBase, "b", nrgba(10, 10))
	c.Add(CompositeKindBase, "c", nrgba(10, 10)) // evicts a

	if got := lookups(CompositeKindBase, "miss") - missBefore; got != 1 {
		t.Errorf("misses: got %v, want 1", got)
	}
	if got := lookups(CompositeKindBase, "hit") - hitBefore; got != 1 {
		t.Errorf("hits: got %v, want 1", got)
	}
	if got := metricValue(t, m.imageCacheEvictions.WithLabelValues(ImageCacheComposite)) - evictBefore; got != 1 {
		t.Errorf("evictions: got %v, want 1", got)
	}
	if got := metricValue(t, m.imageCacheBytes.WithLabelValues(ImageCacheComposite)); got != 800 {
		t.Errorf("bytes gauge: got %v, want 800", got)
	}
	if got := metricValue(t, m.compositeKindBytes.WithLabelValues(CompositeKindBase)); got != 800 {
		t.Errorf("base kind bytes gauge: got %v, want 800", got)
	}
}

func metricValue(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		t.Fatal(err)
	}
	if d.Counter != nil {
		return d.Counter.GetValue()
	}
	return d.Gauge.GetValue()
}

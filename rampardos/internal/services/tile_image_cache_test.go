package services

import (
	"image"
	"testing"
)

func makeTestImage(w, h int) image.Image {
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}

// TestTileImageCacheRoundTrip pins the basic contract: Add + Get
// returns the exact image pointer.
func TestTileImageCacheRoundTrip(t *testing.T) {
	c := NewTileImageCache(4)
	img := makeTestImage(2, 2)
	c.Add("a", img)

	got, ok := c.Get("a")
	if !ok {
		t.Fatal("expected hit on freshly-added key")
	}
	if got != img {
		t.Fatal("expected the same image pointer back")
	}
}

// TestTileImageCacheMiss returns (nil, false) and nothing panics.
func TestTileImageCacheMiss(t *testing.T) {
	c := NewTileImageCache(4)
	got, ok := c.Get("missing")
	if ok {
		t.Fatal("expected miss")
	}
	if got != nil {
		t.Fatal("miss should return nil image")
	}
}

// TestTileImageCacheEvictsOldest locks in LRU eviction: once capacity
// is reached, the least-recently-used entry is dropped first. Accessing
// "a" before inserting the 5th entry keeps it alive and pushes "b" out.
func TestTileImageCacheEvictsOldest(t *testing.T) {
	c := NewTileImageCache(3)
	c.Add("a", makeTestImage(1, 1))
	c.Add("b", makeTestImage(1, 1))
	c.Add("c", makeTestImage(1, 1))

	// Touch "a" so it becomes most-recently-used; "b" is now oldest.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be cached")
	}

	c.Add("d", makeTestImage(1, 1))

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as least-recently-used")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should still be cached", k)
		}
	}
}

// TestTileImageCacheReAddMovesToFront ensures re-adding an existing
// key doesn't grow the LRU and refreshes its recency.
func TestTileImageCacheReAddMovesToFront(t *testing.T) {
	c := NewTileImageCache(2)
	c.Add("a", makeTestImage(1, 1))
	c.Add("b", makeTestImage(1, 1))

	// Re-adding "a" should not evict anything and should make "a"
	// most-recently-used so that the next insertion evicts "b".
	c.Add("a", makeTestImage(1, 1))
	c.Add("c", makeTestImage(1, 1))

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should survive re-add")
	}
}

// TestTileImageCacheSetSizeShrinks pins dynamic resizing: shrinking
// below current length must evict the tail immediately.
func TestTileImageCacheSetSizeShrinks(t *testing.T) {
	c := NewTileImageCache(5)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		c.Add(k, makeTestImage(1, 1))
	}

	c.SetSize(2)

	// Oldest three should be gone; newest two ("d", "e") survive.
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := c.Get(k); ok {
			t.Fatalf("%s should have been evicted on shrink", k)
		}
	}
	for _, k := range []string{"d", "e"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should have survived shrink", k)
		}
	}
}

// TestTileImageCacheDisabledWhenSizeZero: a zero-size cache must
// accept Add calls silently and always return miss — lets operators
// turn the cache off via env without wiring conditional code
// everywhere.
func TestTileImageCacheDisabledWhenSizeZero(t *testing.T) {
	c := NewTileImageCache(0)
	c.Add("a", makeTestImage(1, 1))
	if _, ok := c.Get("a"); ok {
		t.Fatal("zero-size cache should never hit")
	}
}

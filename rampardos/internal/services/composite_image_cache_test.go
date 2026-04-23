package services

import (
	"image"
	"testing"
)

func makeCompositeTestImage(w, h int) image.Image {
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}

func TestCompositeImageCacheRoundTrip(t *testing.T) {
	c := NewCompositeImageCache(4)
	img := makeCompositeTestImage(2, 2)
	c.Add("a", img)

	got, ok := c.Get("a")
	if !ok {
		t.Fatal("expected hit on freshly-added key")
	}
	if got != img {
		t.Fatal("expected the same image pointer back")
	}
}

func TestCompositeImageCacheMiss(t *testing.T) {
	c := NewCompositeImageCache(4)
	if _, ok := c.Get("missing"); ok {
		t.Fatal("expected miss on unknown key")
	}
}

func TestCompositeImageCacheEvictsOldest(t *testing.T) {
	c := NewCompositeImageCache(3)
	c.Add("a", makeCompositeTestImage(1, 1))
	c.Add("b", makeCompositeTestImage(1, 1))
	c.Add("c", makeCompositeTestImage(1, 1))

	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be cached")
	}
	c.Add("d", makeCompositeTestImage(1, 1))

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as least-recently-used")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should still be cached", k)
		}
	}
}

func TestCompositeImageCacheReAddMovesToFront(t *testing.T) {
	c := NewCompositeImageCache(2)
	c.Add("a", makeCompositeTestImage(1, 1))
	c.Add("b", makeCompositeTestImage(1, 1))
	c.Add("a", makeCompositeTestImage(1, 1))
	c.Add("c", makeCompositeTestImage(1, 1))

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should survive re-add")
	}
}

func TestCompositeImageCacheSetSizeShrinks(t *testing.T) {
	c := NewCompositeImageCache(5)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		c.Add(k, makeCompositeTestImage(1, 1))
	}
	c.SetSize(2)
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

func TestCompositeImageCacheDisabledWhenSizeZero(t *testing.T) {
	c := NewCompositeImageCache(0)
	c.Add("a", makeCompositeTestImage(1, 1))
	if _, ok := c.Get("a"); ok {
		t.Fatal("zero-size cache should never hit")
	}
}

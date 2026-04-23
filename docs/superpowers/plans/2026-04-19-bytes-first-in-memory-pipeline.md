# Bytes-First In-Memory Static Map Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the encode/write/read/decode round-trips from the multistaticmap hot path. Images flow in-memory from viewport render → overlay → grid compose → HTTP response. Disk writes happen only for explicit pregenerate requests; a composite image LRU handles cross-request burst sharing within the short-TTL window that makes up poracle traffic.

**Architecture:**

- The renderer returns `*image.NRGBA` directly for viewport and tile renders (skip an internal PNG encode when the caller's next step is a draw).
- **The primary currency through the handler pipeline is `image.Image`, not `[]byte`.** Encoding to PNG/WebP happens once per HTTP response, at the boundary. Multi-composition and LRU storage stay in image form so sharing across requests never triggers a redundant encode+decode cycle.
- `StaticMapHandler.GenerateStaticMap` returns `(image.Image, error)`. Callers that need bytes (HTTP handler) encode once; callers that need pixels (multi composer) use the image directly.
- Inner base-render calls return `image.Image` so the overlay step draws directly on the rendered canvas; no basePath round-trip.
- `MultiStaticMapHandler.handleRequest` collects `[]image.Image` from each component render and hands them to a new in-memory grid composer. **No decode step** — components that hit the cache return their pre-decoded NRGBA directly.
- HTTP responses use `http.ServeContent(…, bytes.NewReader(encoded))` from the in-flight bytes rather than `http.ServeFile(path)`.
- A `CompositeImageCache` LRU (mirrors `TileImageCache`) holds base images and final staticmap images keyed by path so cross-request burst sharing still works without the disk write/read cycle. Stores `image.Image`; the HTTP layer encodes per request. The re-encode cost (~5-10 ms at PNG BestSpeed) is acceptable for the hot case and keeps the cache simple.
- For explicit `pregenerate=true` requests, the handler writes the bytes to disk (kept — the consumer fetches later via `/staticmap/pregenerated/{id}`).
- External-style tile stitching (`generateBaseStaticMapFromTiles`) continues to write basePath to disk (external path touches shared Cache/Tile files and we don't want to rework it in scope); it additionally returns `image.Image` for immediate use.
- `localUseViewport` flag and supporting config stay in place.

**Sharing scenarios the pipeline handles explicitly:**

- **Same basePath, different drawables** (weather alert fan-out: N users, same viewport, per-user marker). `baseSfg` dedupes concurrent base renders, `CompositeImageCache` serves close-in-time repeats. Each user's unique overlay is rendered on the shared in-memory base without touching disk.
- **Shared component panels across multi requests** (weather radar grids: same viewport sections, different user-location markers in other panels). Each component's `GenerateStaticMap` goes through `h.sfg` keyed on `Path()`, so shared components are generated once and cached as `image.Image`. Cross-request hits return the pre-decoded image directly to the grid composer.
- **Shared external tile coverage** across stitched bases. Unchanged — still handled by `TileImageCache` + disk tile cache. External-style basePaths still persist to disk for cross-request stat-lookup (the one concession to keeping tile-stitch plumbing out of scope).

**What doesn't get shared:**

- The final `Path()` for a staticmap with unique markers is unique per user, so the final encoded bytes aren't reused. This is fine — encoding is cheap relative to the generation work.
- `baseSfg` and `h.sfg` serve different roles: the former dedupes concurrent base renders (many final paths share a basePath), the latter dedupes concurrent final-path generations (same URL from multiple clients, including sfg followers).

**Tech Stack:** Go 1.26, chi router, stdlib `image/png`, `golang.org/x/sync/singleflight`, existing `services/renderer` Node worker pool, existing `TileImageCache` LRU pattern.

---

## File Structure

**New files:**
- `rampardos/internal/services/composite_image_cache.go` — LRU of `image.Image` keyed by path; mirrors `tile_image_cache.go` but sized for 400×400 NRGBA. Disabled when size is 0.
- `rampardos/internal/services/composite_image_cache_test.go` — six TDD cases for basic hit/miss/evict/resize, matching the tile LRU test shape.

**Modified files:**
- `rampardos/internal/services/renderer/nodepool.go` — new `RenderViewportImage` method returning `*image.NRGBA`; the existing `RenderViewport` calls it and continues to encode.
- `rampardos/internal/services/metrics.go` — constant `ImageCacheComposite = "composite"`; existing `RecordImageCacheHit/Miss` are reused.
- `rampardos/internal/config/config.go` — `CompositeImageCacheSize` field; env var `COMPOSITE_IMAGE_CACHE_SIZE` default 200.
- `rampardos/cmd/server/main.go` — `InitGlobalCompositeImageCache(cfg.CompositeImageCacheSize)`.
- `rampardos/internal/utils/image_utils_native.go` — new `GenerateStaticMapFromImage`, `GenerateMultiStaticMapFromImages`, and an `EncodeImage` helper; existing file-based wrappers remain for external path compatibility but aren't called by the local-viewport hot path.
- `rampardos/internal/handlers/static_map.go` — `ensureBase`, `generateBaseStaticMap`, `generateBaseStaticMapFromAPI`, `generateBaseStaticMapFromTiles`, `generateStaticMap`, `GenerateStaticMap`, and `handleRequest` all re-shaped to thread `image.Image` and encoded bytes through. `handleRequest` serves via `http.ServeContent`. Pregenerate path writes bytes to disk.
- `rampardos/internal/handlers/multi_static_map.go` — `handleRequest` collects `[]image.Image` and invokes the new compose util; bytes served via `ServeContent`; pregenerate still writes.
- `rampardos/internal/handlers/common.go` — generalize `generateResponse` helpers to accept bytes, or add parallel helpers; `handlePregenerateResponse` accepts bytes and writes to disk.
- `rampardos/internal/handlers/tile.go` — unchanged for now (tile serves still use disk path).
- `CLAUDE.md` — update "Base vs final path" section; add an invariant for the bytes-first pipeline.

**Files intentionally untouched:**
- `rampardos/internal/handlers/tile.go` — the `/tile` endpoint stays disk-backed; tile bytes are small and cache hit rate is 99.9%, so no win.
- External-style tile stitching keeps writing basePath to disk (scope kept tight).

---

## Test Strategy

Race-sensitive tests from PR#10 remain as regression guards:
- `TestEnsureBaseDeletedBetweenCallsDoesNotError` — the stale-index check must still trigger a fresh render. Updated to use the new return types.
- `TestEnsureBaseSingleflightDedupesSiblings` — `baseSfg` dedupes concurrent callers. Updated.
- `TestGenerateStaticMapSingleflightSurvivesLeaderCancel` — `context.WithoutCancel` remains. Updated for new return types.

New tests per task use the `raceTestHandler` pattern already in `static_map_race_test.go`: inject a `renderFn` that records calls and fabricates image.Images deterministically.

---

### Task 1: Add `RenderViewportImage` returning `*image.NRGBA`

**Files:**
- Modify: `rampardos/internal/services/renderer/nodepool.go:164-193`
- Test: `rampardos/internal/services/renderer/nodepool_test.go`

**Why:** Today `RenderViewport` returns PNG-encoded bytes. Every local-style static map then decodes those bytes moments later to overlay drawables. Exposing the `*image.NRGBA` directly skips a pointless encode+decode cycle.

- [ ] **Step 1: Write the failing test**

The `testdata/fake-worker-ok.sh` worker emits a fixed-size canned payload, not a full `width*height*4` RGBA buffer. The existing `TestNodePoolRendererRenderReturnsBytes` passes despite this because `encodeRGBA` has a short-buffer fallback that returns the raw bytes unchanged. The new `RenderViewportImage` deliberately doesn't have that fallback — a malformed buffer from the worker would otherwise silently feed zero pixels to the stitch composer. So the test is a negative one:

Add to `rampardos/internal/services/renderer/nodepool_test.go` just before `TestNodePoolRendererUnknownStyle`:

```go
// TestNodePoolRendererRenderViewportImageRejectsBadPayload pins the
// strict length check in RenderViewportImage. The fake worker
// returns a canned payload that doesn't match width*height*4; the
// strict path must error rather than wrap it as an NRGBA with
// invalid stride.
func TestNodePoolRendererRenderViewportImageRejectsBadPayload(t *testing.T) {
	stylesDir := writeTestStyle(t, "basic")

	npr, err := NewNodePoolRenderer(Config{
		StylesDir:      stylesDir,
		FontsDir:       t.TempDir(),
		MbtilesFile:    "/tmp/fake.mbtiles",
		PoolSize:       1,
		WorkerLifetime: 100,
		RenderTimeout:  5 * time.Second,
		StartupTimeout: 2 * time.Second,
		DiscoverStyles: func() ([]string, error) { return []string{"basic"}, nil },
	}, func(styleID, preparedPath string, ratio int) func() (*worker, error) {
		return func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:           "bash",
				script:           "testdata/fake-worker-ok.sh",
				styleID:          styleID,
				handshakeTimeout: 2 * time.Second,
			})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer npr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = npr.RenderViewportImage(ctx, ViewportRequest{
		StyleID:   "basic",
		Latitude:  51.5,
		Longitude: 0.0,
		Zoom:      14,
		Width:     256,
		Height:    256,
		Scale:     1,
		Format:    models.ImageFormatPNG,
	})
	if err == nil {
		t.Fatal("expected error on short fake-worker payload")
	}
}
```

The "happy path" end-to-end test — real-sized NRGBA from a real maplibre-native render — is exercised by the existing `TestNodePoolRendererRenderReturnsBytes` indirectly (since `RenderViewport` now delegates to `RenderViewportImage`), and by the handler-level race tests which use injected fake renderers. A dedicated happy-path unit test here would require a new fake worker that produces a 256 KB canned buffer — not worth the setup.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd rampardos && go test -run TestNodePoolRendererRenderViewportImageRejectsBadPayload -count=1 -timeout=30s ./internal/services/renderer/`
Expected: `undefined: npr.RenderViewportImage` compile error.

- [ ] **Step 3: Implement `RenderViewportImage`**

Modify `rampardos/internal/services/renderer/nodepool.go`. Replace lines 164-194 (starting with `// RenderViewport dispatches...`) with:

```go
// RenderViewport dispatches an arbitrary viewport request directly,
// returning the encoded bytes in the requested format.
func (npr *NodePoolRenderer) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
	img, err := npr.RenderViewportImage(ctx, req)
	if err != nil {
		return nil, err
	}
	scale := int(req.Scale)
	if scale < 1 {
		scale = 1
	}
	return encodeRGBAImage(img, req.Width*scale, req.Height*scale, req.Format)
}

// RenderViewportImage dispatches a viewport request and returns the
// raw decoded *image.NRGBA. Lets callers skip the PNG encode when
// their next step is a draw operation rather than a disk write.
func (npr *NodePoolRenderer) RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error) {
	scale := int(req.Scale)
	if scale < 1 {
		scale = 1
	}
	pool, err := npr.getOrCreatePool(req.StyleID, req.Scale)
	if err != nil {
		return nil, err
	}
	frame := requestFrameForViewport(req)
	rgba, err := pool.dispatch(ctx, frame)
	if err != nil {
		return nil, err
	}
	width := req.Width * scale
	height := req.Height * scale
	if len(rgba) != width*height*4 {
		return nil, fmt.Errorf("renderer: worker returned %d bytes, expected %d for %dx%d",
			len(rgba), width*height*4, width, height)
	}
	return &image.NRGBA{
		Pix:    rgba,
		Stride: width * 4,
		Rect:   image.Rect(0, 0, width, height),
	}, nil
}

func (npr *NodePoolRenderer) renderViewportInternal(ctx context.Context, req ViewportRequest) ([]byte, error) {
	return npr.RenderViewport(ctx, req)
}
```

Then modify the `encodeRGBA` function at lines 321-354 to also export an image-input variant. Replace the function signature and body:

```go
// encodeRGBA converts raw RGBA bytes to the requested image format.
// If the buffer size does not match width*height*4 (e.g. the fake
// worker's "fake" payload), the raw bytes are returned unchanged.
func encodeRGBA(rgba []byte, width, height int, format models.ImageFormat) ([]byte, error) {
	if len(rgba) != width*height*4 {
		return rgba, nil
	}
	img := &image.NRGBA{
		Pix:    rgba,
		Stride: width * 4,
		Rect:   image.Rect(0, 0, width, height),
	}
	return encodeRGBAImage(img, width, height, format)
}

// encodeRGBAImage is the shared encode path used by both the byte-
// based renderer.Render entry points and any caller that already has
// an *image.NRGBA in hand.
func encodeRGBAImage(img *image.NRGBA, width, height int, format models.ImageFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG:
		encoder := png.Encoder{CompressionLevel: png.BestSpeed}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("renderer: png encode: %w", err)
		}
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("renderer: jpeg encode: %w", err)
		}
	case models.ImageFormatWEBP:
		if err := webp.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("renderer: webp encode: %w", err)
		}
	default:
		return nil, fmt.Errorf("renderer: unsupported format %q", format)
	}
	return buf.Bytes(), nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd rampardos && go test -run TestNodePoolRendererRenderViewportImageRejectsBadPayload -count=1 -timeout=30s ./internal/services/renderer/`
Expected: PASS.

- [ ] **Step 5: Run the full renderer test package to catch regressions**

Run: `cd rampardos && go test -count=1 -timeout=60s ./internal/services/renderer/`
Expected: PASS (with `TestWorkerHangIsKilledOnContextDeadline` flaking intermittently — pre-existing, unrelated).

- [ ] **Step 6: Commit**

```bash
cd /Users/james/GolandProjects/rampardos-tileserver-replacement
git add rampardos/internal/services/renderer/nodepool.go rampardos/internal/services/renderer/nodepool_test.go
git commit -m "$(cat <<'EOF'
feat: RenderViewportImage returns *image.NRGBA directly

Adds a sibling to RenderViewport that skips the internal PNG encode
and returns the decoded *image.NRGBA. Lets the static-map pipeline
overlay drawables directly on the rendered canvas without a
round-trip through PNG bytes.

RenderViewport now calls RenderViewportImage and encodes once via
the shared encodeRGBAImage helper — no behaviour change for callers
that already use RenderViewport.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Composite image LRU

**Files:**
- Create: `rampardos/internal/services/composite_image_cache.go`
- Create: `rampardos/internal/services/composite_image_cache_test.go`
- Modify: `rampardos/internal/services/metrics.go:81-86` (add label constant)

**Why:** Cross-request burst sharing. Within a single request, producer (e.g. `generateBaseStaticMapFromAPI`) and consumer (overlay or grid compose) can pass images via function arguments directly. But two close-in-time requests for the same URL — poracle weather alerts fanned out to many users — need the result to persist across the request boundary. The LRU is that bridge; it replaces the short-TTL disk cache that files existed on disk for anyway.

- [ ] **Step 1: Write the failing tests**

Create `rampardos/internal/services/composite_image_cache_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd rampardos && go test -run TestCompositeImageCache -count=1 -timeout=10s ./internal/services/`
Expected: `undefined: NewCompositeImageCache` compile error.

- [ ] **Step 3: Add the metric label constant**

Edit `rampardos/internal/services/metrics.go`. Find the block with `ImageCacheTile = "tile"` and add one line:

```go
ImageCacheTile      = "tile"
ImageCacheMarker    = "marker"
ImageCacheComposite = "composite"
```

- [ ] **Step 4: Create `composite_image_cache.go`**

Create `rampardos/internal/services/composite_image_cache.go`:

```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd rampardos && go test -run TestCompositeImageCache -count=1 -timeout=10s ./internal/services/`
Expected: PASS on all six tests.

- [ ] **Step 6: Commit**

```bash
git add rampardos/internal/services/composite_image_cache.go rampardos/internal/services/composite_image_cache_test.go rampardos/internal/services/metrics.go
git commit -m "$(cat <<'EOF'
feat: CompositeImageCache LRU for bases and final staticmaps

Sibling to TileImageCache, sized for 400×400 NRGBA images. Same
container/list + map + mutex pattern, same disabled-when-size-0
semantics, same metrics hooks. Used in subsequent tasks to replace
the short-TTL disk cache as the cross-request burst-sharing
mechanism for base renders and final staticmap outputs.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Wire the LRU into config + startup

**Files:**
- Modify: `rampardos/internal/config/config.go`
- Modify: `rampardos/cmd/server/main.go:155-163`

- [ ] **Step 1: Add config field**

Edit `rampardos/internal/config/config.go`. In the struct, near `TileImageCacheSize`, add:

```go
CompositeImageCacheSize int // Max base+staticmap images cached in memory (default: 200, 0 disables). Each entry ~640 KB for 400×400 NRGBA; 200 ≈ 128 MB.
```

In `Load()` near `TileImageCacheSize: getEnvInt("TILE_IMAGE_CACHE_SIZE", 500),`, add:

```go
CompositeImageCacheSize: getEnvInt("COMPOSITE_IMAGE_CACHE_SIZE", 200),
```

- [ ] **Step 2: Initialize in main.go**

Edit `rampardos/cmd/server/main.go`. After `services.InitGlobalTileImageCache(cfg.TileImageCacheSize)`, add:

```go
// Cross-request burst-sharing cache for base renders and final
// staticmap outputs. Replaces the short-TTL disk cache for the
// non-pregenerate hot path.
services.InitGlobalCompositeImageCache(cfg.CompositeImageCacheSize)
```

- [ ] **Step 3: Build to verify**

Run: `cd rampardos && go build ./...`
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add rampardos/internal/config/config.go rampardos/cmd/server/main.go
git commit -m "$(cat <<'EOF'
feat: wire COMPOSITE_IMAGE_CACHE_SIZE into startup

Default 200 entries ≈ 128 MB for 400×400 NRGBA images. 0 disables
the cache. Consumed by the bytes-first pipeline introduced in
subsequent commits; with the cache off, the pipeline still
functions but loses cross-request burst sharing.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: In-memory utility functions for draw + encode

**Files:**
- Modify: `rampardos/internal/utils/image_utils_native.go`

**Why:** The pipeline needs (a) a way to overlay drawables on a base image and return the result as an image, not as a file, and (b) a way to compose a grid from `[]image.Image`. Both exist today but as file-based functions that read basePath / component paths from disk.

- [ ] **Step 1: Read the current file-based implementations**

Read `rampardos/internal/utils/image_utils_native.go` — specifically:
- `GenerateStaticMapNative` (line 126 onwards): takes `basePath` string, calls `loadImage(basePath)`, overlays, calls `saveImage(path, …)`.
- `GenerateMultiStaticMapNative` (line 300 onwards): takes `path` string, loads component files from disk, composes, calls `saveImage(path, result)`.

- [ ] **Step 2: Add `GenerateStaticMapFromImage`**

Insert this function in `rampardos/internal/utils/image_utils_native.go` just before `GenerateStaticMapNative`:

```go
// GenerateStaticMapFromImage renders overlays on top of baseImg and
// returns the resulting image.Image. Unlike GenerateStaticMapNative
// it neither reads from nor writes to disk — the caller owns
// persistence.
func GenerateStaticMapFromImage(staticMap models.StaticMap, baseImg image.Image, sm *SphericalMercator) (image.Image, error) {
	dc := gg.NewContextForImage(baseImg)

	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	for _, polygon := range staticMap.Polygons {
		if len(polygon.Path) == 0 {
			continue
		}
		drawPolygon(dc, staticMap, polygon, sm, scale)
	}
	for _, circle := range staticMap.Circles {
		drawCircle(dc, staticMap, circle, sm, scale)
	}
	for _, marker := range staticMap.Markers {
		if err := drawMarker(dc, staticMap, marker, scale); err != nil {
			slog.Warn("Failed to draw marker", "error", err)
		}
	}

	return dc.Image(), nil
}
```

Then refactor `GenerateStaticMapNative` to delegate:

```go
func GenerateStaticMapNative(staticMap models.StaticMap, basePath, path string, sm *SphericalMercator) error {
	baseImg, err := loadImage(basePath)
	if err != nil {
		return fmt.Errorf("failed to load base image: %w", err)
	}
	img, err := GenerateStaticMapFromImage(staticMap, baseImg, sm)
	if err != nil {
		return err
	}
	return saveImage(path, img)
}
```

This requires extracting the existing polygon/circle/marker draw logic into helper functions. Check the current implementation — if the logic is already in helper functions (`drawPolygon`, `drawCircle`, `drawMarker`), use them. If it's inline in `GenerateStaticMapNative`, extract it verbatim first in a zero-behaviour-change refactor commit.

- [ ] **Step 3: Add `GenerateMultiStaticMapFromImages`**

Insert just before `GenerateMultiStaticMapNative`:

```go
// GenerateMultiStaticMapFromImages composes a grid from already-
// decoded component images. Caller is responsible for the []Image
// ordering matching multiStaticMap.Grid[…].Maps[…] iteration order.
// Returns the composed image; caller owns encoding + persistence.
func GenerateMultiStaticMapFromImages(multiStaticMap models.MultiStaticMap, componentImages []image.Image) (image.Image, error) {
	var groupImages []image.Image
	var groupDirections []models.CombineDirection

	idx := 0
	for _, grid := range multiStaticMap.Grid {
		var composite image.Image
		for _, m := range grid.Maps {
			if idx >= len(componentImages) {
				return nil, fmt.Errorf("component image %d missing (have %d)", idx, len(componentImages))
			}
			img := componentImages[idx]
			idx++
			if m.Direction == models.CombineDirectionFirst || composite == nil {
				composite = img
				continue
			}
			composite = appendImages(composite, img, m.Direction)
		}
		if composite != nil {
			groupImages = append(groupImages, composite)
			groupDirections = append(groupDirections, grid.Direction)
		}
	}
	if len(groupImages) == 0 {
		return nil, fmt.Errorf("no images to combine")
	}
	result := groupImages[0]
	for i := 1; i < len(groupImages); i++ {
		dir := groupDirections[i]
		if dir == models.CombineDirectionFirst {
			dir = models.CombineDirectionRight
		}
		result = appendImages(result, groupImages[i], dir)
	}
	return result, nil
}
```

Refactor `GenerateMultiStaticMapNative` to delegate:

```go
func GenerateMultiStaticMapNative(multiStaticMap models.MultiStaticMap, path string) error {
	var componentImages []image.Image
	for _, grid := range multiStaticMap.Grid {
		for _, m := range grid.Maps {
			mapPath := m.Map.Path()
			img, err := loadImage(mapPath)
			if err != nil {
				return fmt.Errorf("failed to load map %s: %w", mapPath, err)
			}
			componentImages = append(componentImages, img)
		}
	}
	result, err := GenerateMultiStaticMapFromImages(multiStaticMap, componentImages)
	if err != nil {
		return err
	}
	return saveImage(path, result)
}
```

- [ ] **Step 4: Add `EncodeImage` helper**

At the end of the file, add:

```go
// EncodeImage serializes img to the appropriate byte representation
// inferred from pathExt (".png", ".jpg"/".jpeg", ".webp"). Used when
// the pipeline has an image in hand and needs bytes for an HTTP
// response or a disk write.
func EncodeImage(img image.Image, pathExt string) ([]byte, error) {
	var buf bytes.Buffer
	ext := strings.ToLower(pathExt)
	switch ext {
	case ".jpg", ".jpeg":
		quality := 90
		if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
			quality = services.GlobalImageSettings.ImageQuality
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	case ".webp":
		quality := 90
		if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
			quality = services.GlobalImageSettings.ImageQuality
		}
		if err := webp.Encode(&buf, img, webp.Options{Quality: quality}); err != nil {
			return nil, err
		}
	default:
		encoder := &png.Encoder{CompressionLevel: png.BestSpeed}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
```

Add `"bytes"` to the imports at the top of the file.

- [ ] **Step 5: Run the existing utils tests**

Run: `cd rampardos && go test -count=1 -timeout=60s ./internal/utils/`
Expected: PASS (these cover the refactored file-based entry points which must still behave identically).

- [ ] **Step 6: Commit**

```bash
git add rampardos/internal/utils/image_utils_native.go
git commit -m "$(cat <<'EOF'
feat: in-memory image utilities for bytes-first pipeline

Adds three helpers the reshaped handler pipeline will use:

  GenerateStaticMapFromImage        — overlays drawables on an
                                      in-memory base, returns image
  GenerateMultiStaticMapFromImages  — composes a grid from []Image
  EncodeImage                       — serializes an image by ext

Existing file-based entry points (GenerateStaticMapNative,
GenerateMultiStaticMapNative) are preserved for the external-style
tile stitch path; they now delegate to the new helpers internally.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: `generateBaseStaticMap*` returns `image.Image`

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go:393-462`

**Why:** The base render step needs to hand its result to the overlay step in memory.

- [ ] **Step 1: Update the function-valued hook signatures in `StaticMapHandler`**

Replace lines 46-55 (the hook declarations) in `rampardos/internal/handlers/static_map.go`:

```go
	// Function-valued hooks. Production wiring in NewStaticMapHandler
	// sets these to the real methods below; tests override them to
	// record dispatch without touching the renderer or disk.
	generateBaseStaticMapFromAPIFn   func(ctx context.Context, sm models.StaticMap) (image.Image, error)
	generateBaseStaticMapFromTilesFn func(ctx context.Context, sm models.StaticMap, basePath string, extStyle *models.Style) (image.Image, error)
	logExternalViewportApproxFn      func(sm models.StaticMap)
```

(Note: `generateBaseStaticMapFromAPI` no longer needs `basePath` — the caller handles persistence.)

Add `"image"` to the imports.

- [ ] **Step 2: Update `generateBaseStaticMapFromAPI`**

Replace lines 424-462 (the whole function):

```go
func (h *StaticMapHandler) generateBaseStaticMapFromAPI(ctx context.Context, staticMap models.StaticMap) (image.Image, error) {
	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	bearing := 0.0
	if staticMap.Bearing != nil {
		bearing = *staticMap.Bearing
	}
	pitch := 0.0
	if staticMap.Pitch != nil {
		pitch = *staticMap.Pitch
	}

	start := time.Now()
	img, err := h.renderer.RenderViewportImage(ctx, renderer.ViewportRequest{
		StyleID:   staticMap.Style,
		Longitude: staticMap.Longitude,
		Latitude:  staticMap.Latitude,
		Zoom:      staticMap.Zoom,
		Width:     int(staticMap.Width),
		Height:    int(staticMap.Height),
		Bearing:   bearing,
		Pitch:     pitch,
		Scale:     scale,
		Format:    staticMap.GetFormat(),
	})
	services.GlobalMetrics.RecordRendererViewport(staticMap.Style, time.Since(start).Seconds())
	if err != nil {
		return nil, fmt.Errorf("renderer viewport: %w", err)
	}
	return img, nil
}
```

- [ ] **Step 3: Update the Renderer interface in renderer package**

Edit `rampardos/internal/services/renderer/renderer.go` to add `RenderViewportImage` to the `Renderer` interface. Read the file first to find the exact interface block; add a new method in the same style:

```go
RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error)
```

Also add `image` to the renderer.go imports if needed (if not already, add `"image"` — the struct `*image.NRGBA` is returned).

Update the fake renderer in `rampardos/internal/services/renderer/fake.go` to implement the new method. Read the file first; its current `RenderViewport` returns bytes. Add:

```go
func (f *FakeRenderer) RenderViewportImage(ctx context.Context, req ViewportRequest) (*image.NRGBA, error) {
	// Return a 1×1 transparent NRGBA — callers that care about
	// content use the handler-level render hooks anyway.
	return image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil
}
```

Add `"image"` to fake.go imports.

- [ ] **Step 4: Update `generateBaseStaticMapFromTiles` signature**

Replace the function signature at line 464 and the body's final step. This function still writes basePath to disk (external styles rely on it) but now also returns the composed image so the overlay caller can skip a re-read:

Read lines 464 onwards to understand the function. Change the return type from `error` to `(image.Image, error)`. At the point where it currently calls `fileutil.AtomicWriteFile(basePath, encoded, 0o644)` and returns nil, instead:

1. Decode the encoded bytes into an image.
2. Write to disk.
3. Return the image.

The exact patch depends on the function body. The pattern:

```go
// ... existing stitching logic that produces `encoded []byte` ...

if err := fileutil.AtomicWriteFile(basePath, encoded, 0o644); err != nil {
	return nil, err
}

img, _, err := image.Decode(bytes.NewReader(encoded))
if err != nil {
	return nil, fmt.Errorf("decode stitched base: %w", err)
}
return img, nil
```

(One extra decode here is acceptable — external tile stitching is the cold path and we write basePath regardless.)

- [ ] **Step 5: Update `generateBaseStaticMap` dispatcher**

Replace lines 393-413 (the dispatcher):

```go
func (h *StaticMapHandler) generateBaseStaticMap(ctx context.Context, staticMap models.StaticMap, basePath string) (image.Image, error) {
	extStyle := h.stylesController.GetExternalStyle(staticMap.Style)

	if extStyle == nil {
		// Local style: fractional zoom → viewport render (native float zoom).
		// Integer zoom → tile stitching (cacheable via Cache/Tile), unless
		// localUseViewport is set to skip the tile pipeline entirely.
		if isFractional(staticMap.Zoom) || h.localUseViewport {
			return h.generateBaseStaticMapFromAPIFn(ctx, staticMap)
		}
		return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, basePath, extStyle)
	}

	if isFractional(staticMap.Zoom) {
		h.logExternalViewportApproxFn(staticMap)
	}
	return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, basePath, extStyle)
}
```

- [ ] **Step 6: Update `ensureBase` signature and body**

Replace lines 352-360:

```go
func (h *StaticMapHandler) ensureBase(ctx context.Context, staticMap models.StaticMap, basePath string) (image.Image, error) {
	// LRU fast path — covers the cross-request burst-sharing case
	// that the short-TTL disk cache used to handle.
	if services.GlobalCompositeImageCache != nil {
		if img, ok := services.GlobalCompositeImageCache.Get(basePath); ok {
			return img, nil
		}
	}

	v, err, _ := h.baseSfg.Do(basePath, func() (any, error) {
		if services.GlobalCompositeImageCache != nil {
			if img, ok := services.GlobalCompositeImageCache.Get(basePath); ok {
				return img, nil
			}
		}
		img, err := h.generateBaseStaticMap(ctx, staticMap, basePath)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(basePath, img)
		}
		return img, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(image.Image), nil
}
```

- [ ] **Step 7: Update `NewStaticMapHandler` wiring**

In the constructor (around line 58-71), update the hook assignments to use the new signatures:

```go
h.generateBaseStaticMapFromAPIFn = h.generateBaseStaticMapFromAPI
h.generateBaseStaticMapFromTilesFn = h.generateBaseStaticMapFromTiles
h.logExternalViewportApproxFn = h.logExternalViewportApprox
```

(No change needed if the signatures auto-resolve; confirm the method expressions match.)

- [ ] **Step 8: Update `raceTestHandler` in the test file**

Edit `rampardos/internal/handlers/static_map_race_test.go`. The test helper wires `generateBaseStaticMapFromTilesFn` and `generateBaseStaticMapFromAPIFn` to a `renderFn`. Update:

```go
renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) error {
	// ... existing body that writes basePath ...
}
```

Now needs to return `image.Image`. Change the helper signature to match — the renderFn returns `(image.Image, error)`, and each test's renderFn produces an image.

For each existing test renderFn that writes a PNG to basePath and returns nil, change to:

```go
renderFn := func(ctx context.Context, sm models.StaticMap, basePath string) (image.Image, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{A: 255})
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(basePath, raceFakePNG(t), 0o644); err != nil {
		return nil, err
	}
	return img, nil
}
```

And the `raceTestHandler` wiring:

```go
h.generateBaseStaticMapFromTilesFn = func(ctx context.Context, sm models.StaticMap, basePath string, _ *models.Style) (image.Image, error) {
	return renderFn(ctx, sm, basePath)
}
h.generateBaseStaticMapFromAPIFn = func(ctx context.Context, sm models.StaticMap) (image.Image, error) {
	// For the API path there's no basePath — use a computed one for fake writes, or skip the write.
	return renderFn(ctx, sm, sm.BasePath())
}
```

- [ ] **Step 9: Build and run the handler tests**

Run: `cd rampardos && go build ./... && go test -count=1 -timeout=60s ./internal/handlers/`
Expected: PASS on all race and dispatch tests after the signature updates.

- [ ] **Step 10: Commit**

```bash
git add rampardos/internal/handlers/static_map.go rampardos/internal/handlers/static_map_race_test.go rampardos/internal/services/renderer/renderer.go rampardos/internal/services/renderer/fake.go
git commit -m "$(cat <<'EOF'
feat: base render returns image.Image through the pipeline

generateBaseStaticMapFromAPI, generateBaseStaticMapFromTiles,
generateBaseStaticMap, and ensureBase all return image.Image. The
overlay step can now draw directly on the rendered canvas without
reading basePath from disk.

ensureBase consults GlobalCompositeImageCache first (cross-request
burst sharing), then the baseSfg (concurrent dedup), then the
generator. External-style tile stitching still writes basePath to
disk but additionally returns the image so sibling requests in the
same call tree skip the decode.

Test helpers in static_map_race_test.go updated to match the new
signatures; existing race invariants (deletion, singleflight dedup)
preserved.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: `generateStaticMap` consumes base image, returns `image.Image`

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go:362-391`

**Why image, not bytes:** multi composer needs pixels, not PNG bytes. Encoding happens once per HTTP response at the boundary.

- [ ] **Step 1: Rewrite `generateStaticMap`**

Replace lines 362-391:

```go
func (h *StaticMapHandler) generateStaticMap(ctx context.Context, path, basePath string, staticMap models.StaticMap) (image.Image, error) {
	baseImg, err := h.ensureBase(ctx, staticMap, basePath)
	if err != nil {
		return nil, err
	}

	hasDrawables := len(staticMap.Markers) > 0 || len(staticMap.Polygons) > 0 || len(staticMap.Circles) > 0

	if !hasDrawables {
		return baseImg, nil
	}

	if err := h.downloadMarkers(ctx, staticMap); err != nil {
		return nil, err
	}
	return utils.GenerateStaticMapFromImage(staticMap, baseImg, h.sphericalMercator)
}
```

- [ ] **Step 2: Run handler tests**

Run: `cd rampardos && go build ./... && go test -count=1 -timeout=60s ./internal/handlers/`
Expected: the existing tests won't pass yet — callers still use the old signature. The build error is the signal to proceed.

- [ ] **Step 3: Commit (intermediate — build is broken but atomic)**

Actually — defer commit to Task 7 where the callers are fixed. Continue straight to Task 7 without committing.

---

### Task 7: `GenerateStaticMap` returns `image.Image`; sfg.Do passes images

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go:154-193, 245-295`

**Why image, not bytes:** The multi composer consumes `image.Image`. If GenerateStaticMap returned bytes, the multi handler would decode on every cache hit and the LRU would re-encode on every cache hit — two redundant PNG round-trips per shared component. Keeping the pipeline in image-form means cache hits are pure pointer hand-offs; the only encode is at the HTTP boundary.

- [ ] **Step 1: Rewrite `GenerateStaticMap`**

Replace lines 154-193 (the entire public method):

```go
// GenerateStaticMap generates a static map and returns the decoded
// image. Used by MultiStaticMapHandler to skip the PNG round-trip
// that would otherwise happen between component generation and
// grid composition. Concurrent requests for the same map are
// deduplicated via singleflight; the sfg return value is the
// image pointer so followers share the leader's result without
// re-encoding.
//
// Expiry-queue enqueue has been moved out of this function. In the
// bytes-first pipeline nothing is written to disk on this path —
// persistence and its matching expiry enqueue now live exclusively
// in handlePregenerateResponseBytes, the sole disk-write site.
func (h *StaticMapHandler) GenerateStaticMap(ctx context.Context, staticMap models.StaticMap, opts GenerateOpts) (image.Image, error) {
	path := staticMap.Path()
	basePath := staticMap.BasePath()
	_ = basePath // retained for symmetry; ensureBase will key on it

	if !opts.NoCache && services.GlobalCompositeImageCache != nil {
		if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
			return cached, nil
		}
	}

	genCtx := context.WithoutCancel(ctx)
	v, err, _ := h.sfg.Do(path, func() (any, error) {
		if !opts.NoCache && services.GlobalCompositeImageCache != nil {
			if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
				return cached, nil
			}
		}
		img, err := h.generateStaticMap(genCtx, path, basePath, staticMap)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(path, img)
		}
		return img, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(image.Image), nil
}
```

- [ ] **Step 2: Rewrite the `handleRequest` sfg.Do block**

Find lines 245-295 (the `handleRequest` function's `h.sfg.Do` block and the response path). Replace:

```go
	genCtx := context.WithoutCancel(r.Context())
	v, genErr, _ := h.sfg.Do(path, func() (any, error) {
		if !skipCache && services.GlobalCompositeImageCache != nil {
			if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
				return cached, nil
			}
		}
		img, err := h.generateStaticMap(genCtx, path, basePath, staticMap)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(path, img)
		}
		return img, nil
	})

	if genErr != nil {
		slog.Error("Failed to generate static map", "error", genErr)
		services.GlobalMetrics.RecordError("staticmap", "generation_failed")
		http.Error(w, genErr.Error(), http.StatusInternalServerError)
		return
	}
	img := v.(image.Image)
	encoded, err := utils.EncodeImage(img, filepath.Ext(path))
	if err != nil {
		slog.Error("Failed to encode static map", "error", err)
		services.GlobalMetrics.RecordError("staticmap", "encode_failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	duration := time.Since(startTime).Seconds()
	h.statsController.StaticMapServed(true, path, staticMap.Style)
	services.GlobalMetrics.RecordRequest("staticmap", staticMap.Style, false, duration)

	// Resolve effective TTL once. Only used if pregenerate writes
	// the file; otherwise it governs nothing.
	ttl := effectiveTTL(skipCache, ttlSeconds)

	if skipCache {
		slog.Debug("Served static map (nocache)", "file", filepath.Base(path), "duration", duration)
		serveStaticMapBytes(w, r, path, encoded)
		return
	}

	slog.Debug("Served static map (generated)", "file", filepath.Base(path), "duration", duration, "ttl", ttlSeconds)
	h.generateResponse(w, r, staticMap, path, encoded, ttl, basePath)
```

`serveStaticMapBytes` is a new helper:

```go
func serveStaticMapBytes(w http.ResponseWriter, r *http.Request, path string, encoded []byte) {
	w.Header().Set("Cache-Control", "max-age=604800, must-revalidate")
	http.ServeContent(w, r, filepath.Base(path), time.Now(), bytes.NewReader(encoded))
}
```

And `effectiveTTL` resolves the handler's ttlSeconds / skipCache into the duration that `handlePregenerateResponseBytes` will pass to the expiry queue:

```go
// effectiveTTL resolves query-param state into the duration that
// governs disk-file lifetime when pregenerate=true writes a file.
// Only used when there will actually be a file on disk.
func effectiveTTL(skipCache bool, ttlSeconds int) time.Duration {
	if skipCache {
		// The nocache+pregenerate combo is rewritten earlier to
		// skipCache=false + ttlSeconds=30; if skipCache is still
		// true here, there is no pregenerate and no file. The
		// returned value is unused.
		return nocacheBaseTTLFloor
	}
	if ttlSeconds > 0 {
		return time.Duration(ttlSeconds) * time.Second
	}
	return services.OwnedThreshold
}
```

And `generateResponse` takes bytes plus ttl/basePath so the pregenerate helper can enqueue correctly:

```go
func (h *StaticMapHandler) generateResponse(w http.ResponseWriter, r *http.Request, staticMap models.StaticMap, path string, encoded []byte, ttl time.Duration, basePath string) {
	if handlePregenerateResponseBytes(w, r, path, staticMap, encoded, ttl, basePath) {
		return
	}
	serveStaticMapBytes(w, r, path, encoded)
}
```

Add `"bytes"` to the imports.

- [ ] **Step 3: Update `handlePregenerateResponse` to accept bytes and own expiry-queue registration**

Edit `rampardos/internal/handlers/common.go`. Replace `handlePregenerateResponse` with:

```go
// handlePregenerateResponseBytes is the single disk-write site in
// the bytes-first pipeline. When pregenerate=true it writes the
// encoded image to `path` and enqueues the corresponding
// deletion/ownership in the expiry queue. Returns true if
// pregenerate was handled (caller should return).
//
// The enqueue lives here (not at the handler level) because
// writes-to-disk and expiry-registration must be one-to-one —
// enqueueing a path the caller never wrote was a footgun in the
// pre-bytes-first pipeline and no longer exists in this one.
func handlePregenerateResponseBytes(
	w http.ResponseWriter,
	r *http.Request,
	path string,
	data any,
	encoded []byte,
	ttl time.Duration,
	basePath string,
) bool {
	pregenerate := r.URL.Query().Get("pregenerate") == "true"
	if !pregenerate {
		return false
	}

	if err := fileutil.AtomicWriteFile(path, encoded, 0o644); err != nil {
		slog.Error("pregenerate write failed", "path", path, "error", err)
		http.Error(w, "pregenerate failed", http.StatusInternalServerError)
		return true
	}
	enqueueWithBase(services.GlobalExpiryQueue, ttl, path, basePath)

	regeneratable := r.URL.Query().Get("regeneratable") == "true"
	if regeneratable {
		regeneratablePath := fmt.Sprintf("Cache/Regeneratable/%s.json", filepath.Base(path))
		if _, err := os.Stat(regeneratablePath); os.IsNotExist(err) {
			if jsonData, err := json.Marshal(data); err == nil {
				fileutil.AtomicWriteFile(regeneratablePath, jsonData, 0644)
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(filepath.Base(path)))
	return true
}
```

Add `"log/slog"` to the imports if missing.

- [ ] **Step 4: Remove the cached-branch now that LRU handles it**

In `handleRequest`, the existing block at lines 229-246 that does `os.Stat(path)` and serves the file directly becomes unnecessary — the LRU handles it. Delete the block. The flow is now: sfg.Do is always invoked; the callback hits the LRU and returns immediately on a cross-request hit.

Verify: remove the `cached := false` / `if cached { serveFile(...) return }` block entirely. The `cached` label in the metric becomes always-false; that's a temporary regression we accept (existing dashboards split by cached=true vs cached=false may need update).

- [ ] **Step 5: Build and run tests**

Run: `cd rampardos && go build ./... && go test -count=1 -timeout=60s ./internal/handlers/`
Expected: PASS on the race tests (signatures updated in Task 5).

- [ ] **Step 6: Commit**

```bash
git add rampardos/internal/handlers/static_map.go rampardos/internal/handlers/common.go
git commit -m "$(cat <<'EOF'
feat: GenerateStaticMap returns image.Image; sfg passes images

Public GenerateStaticMap now returns (image.Image, error). The
sfg.Do callbacks propagate image pointers so followers share the
leader's pre-decoded NRGBA without re-encoding. Encoding to PNG
happens once at the HTTP boundary, right before ServeContent.

Keeping the pipeline's currency as image.Image (not []byte) matters
for the multistaticmap case, where the grid composer consumes
pixels. If we returned bytes, shared components hitting the LRU
would pay an encode+decode per request even though no new work is
needed.

Pregenerate is the only path that writes to disk — explicit user
opt-in via ?pregenerate=true. All other requests flow through the
CompositeImageCache LRU for cross-request image reuse.

ExpiryQueue.Add calls are centralised in handlePregenerateResponseBytes
alongside the AtomicWriteFile, enforcing a one-to-one invariant:
every disk-file write has a matching expiry registration, and no
other code path enqueues phantom paths that were never written. The
handler resolves the effective TTL via a small helper and passes it
through the response stack.

Accepted trade-off: no-TTL requests (default OwnedThreshold) no
longer benefit from a 7-day disk fallback on duplicate responses.
Reuse is LRU-bounded (~minutes at 200 slots). Poracle's burst-share
window fits comfortably inside the LRU; long-tail reuse regenerates
at ~50 ms / ~150 ms per staticmap / multi.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: Multi handler collects images, composes in-memory (no decode)

**Files:**
- Modify: `rampardos/internal/handlers/multi_static_map.go:150-244`

**Why no decode:** `GenerateStaticMap` now returns `image.Image`. The grid composer takes `[]image.Image`. Shared components that hit the `CompositeImageCache` return their pre-decoded NRGBA directly — no PNG round-trip for cache hits across multi requests.

- [ ] **Step 1: Rewrite the multi sfg.Do block**

Find the block at lines 172-223. Replace with:

```go
	genCtx := context.WithoutCancel(r.Context())
	v, sfErr, _ := h.sfg.Do(path, func() (any, error) {
		if !skipCache && services.GlobalCompositeImageCache != nil {
			if cached, ok := services.GlobalCompositeImageCache.Get(path); ok {
				return cached, nil
			}
		}

		// Parallel component generation — each returns *image.Image*
		// directly. Shared components (e.g. weather radar panels
		// reused across users' multistaticmap requests) hit the
		// composite LRU and come back pre-decoded; unique ones are
		// generated and cached in image form.
		componentImages := make([]image.Image, len(mapsToGenerate))
		var wg sync.WaitGroup
		var genErr error
		var errOnce sync.Once
		sem := make(chan struct{}, 5)

		for i, staticMap := range mapsToGenerate {
			wg.Add(1)
			go func(idx int, sm models.StaticMap) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				img, err := h.staticMapHandler.GenerateStaticMap(genCtx, sm, componentOpts)
				if err != nil {
					errOnce.Do(func() { genErr = err })
					return
				}
				componentImages[idx] = img
			}(i, staticMap)
		}
		wg.Wait()

		if genErr != nil {
			return nil, genErr
		}

		composed, err := utils.GenerateMultiStaticMapFromImages(multiStaticMap, componentImages)
		if err != nil {
			return nil, err
		}
		if services.GlobalCompositeImageCache != nil {
			services.GlobalCompositeImageCache.Add(path, composed)
		}
		return composed, nil
	})

	if sfErr != nil {
		slog.Error("Failed to generate multi-static map", "error", sfErr)
		services.GlobalMetrics.RecordError("multistaticmap", "generation_failed")
		http.Error(w, sfErr.Error(), http.StatusInternalServerError)
		return
	}
	composed := v.(image.Image)
	encoded, err := utils.EncodeImage(composed, filepath.Ext(path))
	if err != nil {
		slog.Error("Failed to encode multi-static map", "error", err)
		services.GlobalMetrics.RecordError("multistaticmap", "encode_failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

- [ ] **Step 2: Update the response path in `handleRequest`**

Replace the remainder of `handleRequest` starting from the `duration := …` line:

```go
	duration := time.Since(startTime).Seconds()
	h.statsController.StaticMapServed(true, path, "multi")
	services.GlobalMetrics.RecordRequest("multistaticmap", "multi", false, duration)

	ttl := effectiveTTL(skipCache, ttlSeconds)

	if skipCache {
		slog.Debug("Served multi-static map (nocache)", "file", filepath.Base(path), "maps", mapCount, "duration", duration)
		serveStaticMapBytes(w, r, path, encoded)
		return
	}

	slog.Debug("Served multi-static map (generated)", "file", filepath.Base(path), "maps", mapCount, "duration", duration, "ttl", ttlSeconds)
	h.generateResponse(w, r, multiStaticMap, path, encoded, ttl, path)
```

(`basePath` is passed as `path` because multistaticmap has no shared base-across-overlays concept — the grid itself is the shared artefact, and `enqueueWithBase` already handles `path == basePath` by issuing a single-path enqueue.)

Remove the early-cache block at lines 145-153 (the `os.Stat(path)` check) for the same reason as the static_map change — LRU handles it.

Update `generateResponse` in `multi_static_map.go` to accept bytes and TTL, matching the static_map pattern:

```go
func (h *MultiStaticMapHandler) generateResponse(w http.ResponseWriter, r *http.Request, multiStaticMap models.MultiStaticMap, path string, encoded []byte, ttl time.Duration, basePath string) {
	if handlePregenerateResponseBytes(w, r, path, multiStaticMap, encoded, ttl, basePath) {
		return
	}
	serveStaticMapBytes(w, r, path, encoded)
}
```

Add `"image"` to the imports (no `"bytes"` needed here — Task 7's static_map edit already added it at the package level).

- [ ] **Step 3: Build and run full test suite**

Run: `cd rampardos && go build ./... && go test -count=1 -timeout=120s ./...`
Expected: PASS (except pre-existing `TestWorkerHangIsKilledOnContextDeadline` flake).

- [ ] **Step 4: Commit**

```bash
git add rampardos/internal/handlers/multi_static_map.go
git commit -m "$(cat <<'EOF'
feat: multistaticmap composes from in-memory component images

Multi handler collects []image.Image directly from each component's
GenerateStaticMap call (no decode step) and hands them to
GenerateMultiStaticMapFromImages. Shared components across multi
requests (e.g. weather radar panels common to many users) hit the
CompositeImageCache and return as pre-decoded NRGBA without any
PNG round-trip.

Response served via ServeContent from the grid encode (one encode
per request, at the HTTP boundary).

Saves N decodes per multistaticmap request (one per component) plus
the component disk write/read round-trip. For N=15 component multis
this was the bulk of the remaining 103 ms latency; cross-request
component sharing additionally compounds the win for the weather-
alert fan-out workload.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: Remove the tile-style stitcher's `saveImage` for local path; keep for external

**Files:**
- Modify: `rampardos/internal/utils/image_utils_native.go` (review remaining callers)

**Why:** Local-style handlers now take the viewport path and don't touch the stitcher. The stitcher's `GenerateBaseStaticMapNative` still saves to basePath for external styles — keep it. Just verify no regression.

- [ ] **Step 1: Audit callers**

Run: `cd rampardos && grep -rn 'GenerateBaseStaticMap\|GenerateStaticMapNative\|GenerateMultiStaticMapNative' --include='*.go'`

Expected:
- `GenerateBaseStaticMap` called only from `generateBaseStaticMapFromTiles` (external-style path) — OK, unchanged.
- `GenerateStaticMapNative` no longer called from the handler (refactored in Task 6). If tests reference it, they can keep it as a file-based convenience for other integration tests.
- `GenerateMultiStaticMapNative` no longer called from the handler (refactored in Task 8). Same status as above.

- [ ] **Step 2: Verify tests still exercise both paths**

Run: `cd rampardos && go test -count=1 -timeout=60s ./internal/utils/`
Expected: PASS.

- [ ] **Step 3: No commit needed (no code changes)**

---

### Task 10: Update CLAUDE.md to reflect the bytes-first pipeline

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Rewrite the "Base vs final path" section**

Edit `CLAUDE.md`. Replace the "Base vs final path" section with:

```markdown
## Base vs final path (image-first, in-memory)

- The hot path's primary currency is `image.Image`, not bytes.
  `ensureBase` returns `image.Image`; `generateStaticMap` and the
  public `GenerateStaticMap` return `image.Image`; the multi
  composer consumes `[]image.Image` from component calls. Encoding
  happens once per HTTP response at the boundary via
  `http.ServeContent(…, bytes.NewReader(encoded))`. No disk
  round-trip on the critical path for local-style requests.
- `CompositeImageCache` (services.GlobalCompositeImageCache) is
  the cross-request burst-sharing mechanism. Keyed by path. Holds
  base renders and final staticmap outputs as `image.Image`. Size
  via `COMPOSITE_IMAGE_CACHE_SIZE` (default 200).
- Shared bases (N users, same viewport, different drawables) and
  shared components (multi requests reusing panel viewports) both
  hit this cache without triggering any PNG round-trip. The
  encode-on-serve cost is the only encode in the non-pregenerate
  hot path.
- `baseSfg` dedupes concurrent base renders; the outer `sfg`
  dedupes final-path generation. Followers share the leader's
  image pointer via the sfg return value.
- Disk writes happen only for `?pregenerate=true`. External-style
  (Mapbox) tile stitching still writes basePath to disk because its
  inner tile cache lives on disk.
- `staticMap.BasePath()` is the LRU key for base images;
  `staticMap.Path()` for final staticmaps. Both stable across
  process lifetime.
- **Enqueue invariant:** `ExpiryQueue.Add` is only called from
  `handlePregenerateResponseBytes`, right next to the
  `AtomicWriteFile`. Every disk file has exactly one matching
  expiry registration. Handler code above does not call
  `enqueueWithBase` — adding one for a file that was never written
  was a latent footgun in the pre-bytes-first pipeline.
- **No disk fallback for long-tail reuse.** Requests without a TTL
  no longer benefit from a 7-day disk cache on duplicate responses.
  Reuse is LRU-bounded (~minutes at 200 slots). Accepted because
  poracle's burst-share window fits inside the LRU; long-window
  regeneration cost is ~50 ms single / ~150 ms multi.
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "$(cat <<'EOF'
docs: update CLAUDE.md for bytes-first pipeline

Rewrites the "Base vs final path" section to document the in-memory
contract, the CompositeImageCache LRU role, and the pregenerate-only
disk write invariant.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Load test + observe

**Why:** The point of this refactor was latency. Validate against the measured 103 ms baseline.

- [ ] **Step 1: Build and deploy the branch to the staging server**

- [ ] **Step 2: Let it run with production-equivalent traffic for 30+ min**

- [ ] **Step 3: Compare the multistaticmap latency**

```bash
curl -s http://localhost:9000/metrics \
  | grep -E '^rampardos_request_duration_seconds_(count|sum)' | grep multi
```

Divide `_sum/_count`. Baseline was ~103 ms (commit `0b7ec63`). Target: ~50-70 ms with the new pipeline at similar traffic shape.

- [ ] **Step 4: Check the composite cache hit rate**

```bash
curl -s http://localhost:9000/metrics | grep 'rampardos_image_cache_.*composite'
```

Expected hit rate above ~50% after a few minutes of traffic (depends on poracle's burst-sharing pattern). Bump `COMPOSITE_IMAGE_CACHE_SIZE` if low.

- [ ] **Step 5: Check renderer pool saturation**

Same monitoring as set up in `edc2eff`: `rampardos_renderer_pool_acquire_wait_seconds` and `_idle_workers`. Sustained idle=0 or p95 wait > 100 ms means the Node pool is saturated — stop here, increase pool size or revert.

- [ ] **Step 6: Capture a pprof and compare to the edc2eff baseline**

```bash
curl -o pprof-after-bytesfirst.out http://localhost:9000/debug/pprof/profile?seconds=60
```

Expected shape: `png.writeImage` still the top cum (final grid encode is unavoidable), but `readImagePass` / `loadImage` / `image.Decode` should all be effectively gone from the hot path — no more file-read decodes for base or components.

- [ ] **Step 7: If numbers look good, merge to `tileserver-replacement` and push upstream**

```bash
git checkout tileserver-replacement
git merge --ff-only bytes-first-in-memory-pipeline
git push upstream tileserver-replacement
```

If numbers regress unexpectedly, leave the branch unmerged and compare against the plan's assumptions — the renderer saturation metrics should indicate whether the Node pool is the bottleneck.

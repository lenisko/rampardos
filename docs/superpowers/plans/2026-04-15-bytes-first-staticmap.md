# Bytes-First Static Map Generation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `/staticmap` and `/multistaticmap` race-free and tolerant of external deletion of intermediate files by passing image bytes through the pipeline instead of re-reading disk.

**Architecture:** The handler reads the base image into memory **exactly once** per request (or renders it in-memory on cache miss). Marker images are likewise held in memory between download and overlay-draw, so an admin cache-drop between those steps cannot fail the request. Overlays are drawn in memory, final bytes are written atomically. The cache index is no longer consulted for base existence (eliminating a stale-index race). The expiry queue no longer tracks `basePath` — the shared-base lifetime is owned by `CacheCleaner`'s age-based sweep. The multi-map stitcher composes from `[][]byte` returned by component generation, so components cannot disappear mid-stitch.

**Scope note:** `Cache/Tile/*` is left on the disk-read path. Tile files get fresh mtime on write (age-based eviction can't touch them during the sub-second stitch window) and `loadImageWithRetry` already handles ENOENT via the `redownload` callback. The marker path has no retry equivalent, so markers graduate to bytes-in-memory; tiles do not.

**Tech Stack:** Go 1.26, `golang.org/x/sync/singleflight`, `image`/`image/png`/`image/jpeg`/`image/webp`, existing `ExpiryQueue`, `CacheCleaner`, `CacheIndex`.

**Branch:** `fix-race-bytes-first` off `tileserver-replacement`.

---

## File Structure

| File | Responsibility | Change |
|------|---------------|--------|
| `rampardos/internal/utils/image_utils.go` | Public entry points for image ops | Add `GenerateStaticMapBytes`, `ComposeMultiStaticMapBytes`. Keep legacy file-path wrappers as thin shims during migration, then remove. |
| `rampardos/internal/utils/image_utils_native.go` | Native Go impls | Add `generateStaticMapBytesNative`, `composeMultiStaticMapBytesNative`, image decode/encode helpers. Leave `GenerateBaseStaticMapNative` signature alone but have it also expose a bytes form internally. |
| `rampardos/internal/utils/image_bytes.go` | **new** — decode/encode helpers | `decodeImage(b []byte) (image.Image, error)`, `encodeImage(img image.Image, format models.ImageFormat) ([]byte, error)`. |
| `rampardos/internal/handlers/static_map.go` | Static-map HTTP flow | Rewrite `generateStaticMap` / `GenerateStaticMap` to return `[]byte`. Add per-basePath singleflight. Remove `baseExists` pre-check. Stop enqueueing `basePath` in expiry queue. Replace `downloadMarkers` with `downloadMarkerBytes` returning a `map[string][]byte` keyed by marker cache path. |
| `rampardos/internal/handlers/multi_static_map.go` | Multi-map HTTP flow | Rewrite generate+stitch to operate on `[][]byte`. Remove nocache component-cleanup loop. |
| `rampardos/internal/handlers/static_map_race_test.go` | **new** — race tests | TestExternalDeletionDuringRender, TestConcurrentSiblingBaseSingleflight. |
| `rampardos/internal/utils/image_utils_native_test.go` | Existing image tests | Add tests for new byte-form functions. |

---

## Task 0: Branch + baseline

**Files:** none modified; just branch + verify clean state.

- [ ] **Step 1: Create branch off tileserver-replacement**

```bash
git checkout tileserver-replacement
git pull upstream tileserver-replacement
git checkout -b fix-race-bytes-first
```

- [ ] **Step 2: Run full test suite to confirm baseline green**

```bash
cd rampardos && go test ./...
```

Expected: all packages pass.

- [ ] **Step 3: Run the existing race-shaped handler tests to confirm baseline**

```bash
cd rampardos && go test -race ./internal/handlers/...
```

Expected: PASS.

---

## Task 1: Image decode/encode helpers

**Files:**
- Create: `rampardos/internal/utils/image_bytes.go`
- Test: `rampardos/internal/utils/image_bytes_test.go` (new)

- [ ] **Step 1: Write failing test**

Create `rampardos/internal/utils/image_bytes_test.go`:

```go
package utils

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})

	b, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeImage(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bounds() != img.Bounds() {
		t.Fatalf("bounds mismatch: got %v want %v", got.Bounds(), img.Bounds())
	}

	// Sanity: re-encode and parse as PNG to confirm format.
	var buf bytes.Buffer
	if err := png.Encode(&buf, got); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
}

func TestDecodeInvalidBytes(t *testing.T) {
	if _, err := decodeImage([]byte("not an image")); err == nil {
		t.Fatal("expected error decoding garbage")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd rampardos && go test ./internal/utils/ -run TestEncodeDecodeRoundTrip -v
```

Expected: FAIL with undefined `encodeImage`/`decodeImage`.

- [ ] **Step 3: Implement helpers**

Create `rampardos/internal/utils/image_bytes.go`:

```go
package utils

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
)

// decodeImage decodes PNG/JPEG/WEBP/GIF bytes via the stdlib image
// registry (format detection is handled by the imported decoders).
func decodeImage(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}

// encodeImage encodes img in the requested format.
func encodeImage(img image.Image, format models.ImageFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG, "":
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("png encode: %w", err)
		}
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("jpeg encode: %w", err)
		}
	case models.ImageFormatWEBP:
		if err := webp.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("webp encode: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported image format %q", format)
	}
	return buf.Bytes(), nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd rampardos && go test ./internal/utils/ -run TestEncodeDecodeRoundTrip -v
cd rampardos && go test ./internal/utils/ -run TestDecodeInvalidBytes -v
```

Expected: PASS for both.

- [ ] **Step 5: Commit**

```bash
git add rampardos/internal/utils/image_bytes.go rampardos/internal/utils/image_bytes_test.go
git commit -m "feat(utils): add image decode/encode byte helpers"
```

---

## Task 2: `GenerateStaticMapBytes` — in-memory overlay drawing

**Files:**
- Modify: `rampardos/internal/utils/image_utils.go` (add wrapper)
- Modify: `rampardos/internal/utils/image_utils_native.go` (add native impl, refactor `GenerateStaticMapNative` to delegate)

- [ ] **Step 1: Write failing test**

Append to `rampardos/internal/utils/image_bytes_test.go`:

```go
func TestGenerateStaticMapBytes_NoDrawables(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	baseBytes, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sm := models.StaticMap{Width: 8, Height: 8, Zoom: 10, Latitude: 0, Longitude: 0, Format: models.ImageFormatPNG}

	out, err := GenerateStaticMapBytes(sm, baseBytes, nil, NewSphericalMercator())
	if err != nil {
		t.Fatalf("GenerateStaticMapBytes: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty output")
	}
	if _, err := decodeImage(out); err != nil {
		t.Fatalf("output not decodable: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd rampardos && go test ./internal/utils/ -run TestGenerateStaticMapBytes_NoDrawables -v
```

Expected: FAIL with undefined `GenerateStaticMapBytes`.

- [ ] **Step 3: Add exported wrapper**

In `rampardos/internal/utils/image_utils.go`, after the existing `GenerateStaticMap`:

```go
// GenerateStaticMapBytes draws overlays (markers/polygons/circles) onto
// an already-encoded base image in memory and returns the encoded result.
// No disk access — callers hold both the base bytes and the marker bytes
// (keyed by the marker's cache path from getMarkerPath), so external
// file deletion between download and draw cannot affect this path.
// markerBytes may be nil when the staticMap has no markers.
func GenerateStaticMapBytes(staticMap models.StaticMap, baseBytes []byte, markerBytes map[string][]byte, sm *SphericalMercator) ([]byte, error) {
	return generateStaticMapBytesNative(staticMap, baseBytes, markerBytes, sm)
}
```

- [ ] **Step 4: Add native impl and refactor file-path variant to delegate**

In `rampardos/internal/utils/image_utils_native.go`, replace `GenerateStaticMapNative` with:

```go
// GenerateStaticMapNative is the file-path variant kept for callers that
// still operate on disk. It delegates to the bytes variant.
func GenerateStaticMapNative(staticMap models.StaticMap, basePath, path string, sm *SphericalMercator) error {
	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		return fmt.Errorf("failed to load base image: %w", err)
	}
	// Legacy file-path callers pass nil marker bytes; drawOverlays
	// falls back to disk reads via the existing loadImage path.
	out, err := generateStaticMapBytesNative(staticMap, baseBytes, nil, sm)
	if err != nil {
		return err
	}
	return saveImageBytes(path, out)
}

// generateStaticMapBytesNative is the canonical in-memory implementation.
// It decodes the base, draws overlays using gg, and encodes to the
// requested format.
func generateStaticMapBytesNative(staticMap models.StaticMap, baseBytes []byte, markerBytes map[string][]byte, sm *SphericalMercator) ([]byte, error) {
	baseImg, err := decodeImage(baseBytes)
	if err != nil {
		return nil, fmt.Errorf("decode base: %w", err)
	}

	dc := gg.NewContextForImage(baseImg)

	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	// Extract the polygon/circle/marker drawing loop from the current
	// GenerateStaticMapNative body (lines ~132-290) into drawOverlays.
	// Marker reads go via markerBytes if present (in-memory); otherwise
	// fall back to loadImage(getMarkerPath(marker)) so the legacy
	// file-path wrapper still works.
	if err := drawOverlays(dc, staticMap, scale, markerBytes, sm); err != nil {
		return nil, err
	}

	format := staticMap.Format
	if format == "" {
		format = models.ImageFormatPNG
	}
	return encodeImage(dc.Image(), format)
}
```

Extract the existing overlay-drawing body into a helper `drawOverlays(dc *gg.Context, sm models.StaticMap, scale uint8, merc *SphericalMercator) error` so it can be shared. Move lines currently at `image_utils_native.go:132-290` (polygon/circle/marker processing) into the helper.

Add `saveImageBytes(path string, data []byte) error` that is the byte-equivalent of `saveImage` (`os.MkdirAll` + `os.CreateTemp` + `os.Rename`):

```go
func saveImageBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run all util tests**

```bash
cd rampardos && go test ./internal/utils/
```

Expected: PASS (existing `leaf_test.go`, `leaf_to_jet_test.go`, new bytes tests, and the file-path `GenerateStaticMapNative` tests all pass — the refactor is pure behaviour-preserving).

- [ ] **Step 6: Commit**

```bash
git add rampardos/internal/utils/image_utils.go rampardos/internal/utils/image_utils_native.go rampardos/internal/utils/image_bytes_test.go
git commit -m "feat(utils): bytes-first GenerateStaticMapBytes with file-path wrapper"
```

---

## Task 3: `generateBaseStaticMap*` return bytes

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go` (methods `generateBaseStaticMapFromAPI`, `generateBaseStaticMapFromTiles`, `generateBaseStaticMap`)

**Context:** Currently these methods write directly to `basePath`. Refactor so they **return bytes** and let the caller decide whether to persist. This lets the single-map flow hold bytes in memory without re-reading.

- [ ] **Step 1: Change signatures — return `([]byte, error)`**

In `rampardos/internal/handlers/static_map.go`:

```go
// generateBaseStaticMap renders the base image (without overlays) and
// returns the encoded bytes. Callers are responsible for persisting if
// they want caching.
func (h *StaticMapHandler) generateBaseStaticMap(ctx context.Context, staticMap models.StaticMap) ([]byte, error) {
	extStyle := h.stylesController.GetExternalStyle(staticMap.Style)

	if extStyle == nil {
		if isFractional(staticMap.Zoom) {
			return h.generateBaseStaticMapFromAPIFn(ctx, staticMap)
		}
		return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, extStyle)
	}
	// External style: fractional zoom uses external proxy, integer uses tiles.
	if isFractional(staticMap.Zoom) {
		return h.generateBaseStaticMapFromAPIFn(ctx, staticMap)
	}
	return h.generateBaseStaticMapFromTilesFn(ctx, staticMap, extStyle)
}
```

Update the hook types:

```go
generateBaseStaticMapFromAPIFn   func(ctx context.Context, sm models.StaticMap) ([]byte, error)
generateBaseStaticMapFromTilesFn func(ctx context.Context, sm models.StaticMap, extStyle *models.Style) ([]byte, error)
```

- [ ] **Step 2: Update `generateBaseStaticMapFromAPI` body to return bytes**

Find the existing method (renders via `renderer.RenderViewport` then writes to disk). Change the tail from "write to `basePath`" to "return the rendered bytes". The renderer already returns `[]byte` from `Render`/`RenderViewport`, so the change is: drop the `os.WriteFile(basePath, data, 0644)` / `atomicWriteFile` call and `return data, nil` instead.

- [ ] **Step 3: Update `generateBaseStaticMapFromTiles` body to return bytes**

This method currently composes tiles into an image and writes `basePath`. Refactor to call a new `utils.ComposeBaseStaticMapBytes(...)` that returns `[]byte`. Add that new utility in `rampardos/internal/utils/image_utils.go`:

```go
// ComposeBaseStaticMapBytes stitches and crops tile images into the
// final base, returning encoded bytes. Equivalent to
// GenerateBaseStaticMap but without touching disk.
func ComposeBaseStaticMapBytes(staticMap models.StaticMap, tilePaths []string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) ([]byte, error) {
	return composeBaseStaticMapBytesNative(staticMap, tilePaths, offsetX, offsetY, hasScale, redownload)
}
```

In `rampardos/internal/utils/image_utils_native.go`, extract lines `27-123` of the existing `GenerateBaseStaticMapNative` into `composeBaseStaticMapBytesNative` that returns `([]byte, error)` (replacing the final `return saveImage(path, cropped)` with `return encodeImage(cropped, format)`). Have `GenerateBaseStaticMapNative` become a thin shim that calls the bytes form then `saveImageBytes`:

```go
func GenerateBaseStaticMapNative(staticMap models.StaticMap, tilePaths []string, path string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) error {
	b, err := composeBaseStaticMapBytesNative(staticMap, tilePaths, offsetX, offsetY, hasScale, redownload)
	if err != nil {
		return err
	}
	return saveImageBytes(path, b)
}
```

- [ ] **Step 4: Update caller wiring in `NewStaticMapHandler`**

No change needed — the hooks still point to methods; only the signatures change. Verify that the assignments in `NewStaticMapHandler` still compile after the signature change.

- [ ] **Step 5: Run test suite**

```bash
cd rampardos && go test ./...
```

Expected: PASS. Any existing handler tests that override `generateBaseStaticMap*Fn` need their hook signatures updated to match.

- [ ] **Step 6: Commit**

```bash
git add rampardos/internal/handlers/static_map.go rampardos/internal/utils/image_utils.go rampardos/internal/utils/image_utils_native.go
git commit -m "refactor(handlers): base static map generators return bytes"
```

---

## Task 4: Bytes-first `generateStaticMap` with per-basePath singleflight

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go`

- [ ] **Step 1: Add basePath singleflight field**

In the `StaticMapHandler` struct, add a second singleflight group dedicated to base rendering:

```go
type StaticMapHandler struct {
	// ...existing fields...
	sfg     singleflight.Group // dedupe final path
	baseSfg singleflight.Group // dedupe base rendering for concurrent siblings
	// ...
}
```

- [ ] **Step 2: Add `loadOrRenderBaseBytes` helper**

```go
// loadOrRenderBaseBytes returns the base image for staticMap. If
// basePath exists on disk, its bytes are read once. Otherwise the
// base is rendered in memory and best-effort persisted. Concurrent
// callers for the same basePath are deduplicated.
func (h *StaticMapHandler) loadOrRenderBaseBytes(ctx context.Context, staticMap models.StaticMap, basePath string) ([]byte, error) {
	v, err, _ := h.baseSfg.Do(basePath, func() (any, error) {
		if b, err := os.ReadFile(basePath); err == nil {
			return b, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read base: %w", err)
		}
		b, err := h.generateBaseStaticMap(ctx, staticMap)
		if err != nil {
			return nil, err
		}
		// Best-effort persist. Failure is non-fatal: the request
		// succeeds with bytes in hand; cache miss penalty falls on the
		// next request.
		if writeErr := utils.SaveImageBytes(basePath, b); writeErr != nil {
			slog.Warn("Failed to cache base image", "path", basePath, "error", writeErr)
		}
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}
```

(Export `saveImageBytes` as `SaveImageBytes` in Task 2 — update that task's Step 4 to use the exported name. Already consistent below.)

- [ ] **Step 3: Rewrite `generateStaticMap` to return bytes**

Replace the existing `generateStaticMap` (lines ~365-398) with:

```go
// generateStaticMap produces the final static map bytes and, on cache
// paths, persists them. Returns the final bytes so the caller can
// serve from memory without a second disk read (closing the last race
// window between write and serve).
func (h *StaticMapHandler) generateStaticMap(ctx context.Context, path, basePath string, staticMap models.StaticMap) ([]byte, error) {
	hasDrawables := len(staticMap.Markers) > 0 || len(staticMap.Polygons) > 0 || len(staticMap.Circles) > 0

	baseBytes, err := h.loadOrRenderBaseBytes(ctx, staticMap, basePath)
	if err != nil {
		return nil, err
	}

	// No drawables: final image == base image.
	if !hasDrawables {
		if path != basePath {
			if err := utils.SaveImageBytes(path, baseBytes); err != nil {
				return nil, err
			}
		}
		return baseBytes, nil
	}

	markerBytes, err := h.downloadMarkerBytes(ctx, staticMap)
	if err != nil {
		return nil, err
	}

	finalBytes, err := utils.GenerateStaticMapBytes(staticMap, baseBytes, markerBytes, h.sphericalMercator)
	if err != nil {
		return nil, err
	}
	if err := utils.SaveImageBytes(path, finalBytes); err != nil {
		return nil, err
	}
	return finalBytes, nil
}
```

- [ ] **Step 4: Update `handleRequest` to use new signature**

In `handleRequest` (lines 266-282 area), replace:

```go
_, genErr, _ := h.sfg.Do(path, func() (any, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, nil
	}
	baseExists := false
	if services.GlobalCacheIndex != nil && services.GlobalCacheIndex.HasStaticMap(basePath) {
		baseExists = true
	} else if _, err := os.Stat(basePath); err == nil {
		baseExists = true
		if services.GlobalCacheIndex != nil {
			services.GlobalCacheIndex.AddStaticMap(basePath)
		}
	}
	return nil, h.generateStaticMap(r.Context(), path, basePath, baseExists, staticMap)
})
```

with:

```go
v, genErr, _ := h.sfg.Do(path, func() (any, error) {
	if _, err := os.Stat(path); err == nil {
		return os.ReadFile(path)
	}
	return h.generateStaticMap(r.Context(), path, basePath, staticMap)
})

var finalBytes []byte
if genErr == nil && v != nil {
	finalBytes, _ = v.([]byte)
}
```

Note: the cache index is still updated *after* a successful generate (existing code path), but we no longer consult it for base existence. The index now purely signals "final path is on disk" for the fast path at line ~243.

- [ ] **Step 5: Update `GenerateStaticMap` (exported, multi-map entry) to return bytes**

Change:

```go
func (h *StaticMapHandler) GenerateStaticMap(ctx context.Context, staticMap models.StaticMap, opts GenerateOpts) ([]byte, error)
```

Body:

```go
func (h *StaticMapHandler) GenerateStaticMap(ctx context.Context, staticMap models.StaticMap, opts GenerateOpts) ([]byte, error) {
	path := staticMap.Path()
	basePath := staticMap.BasePath()

	if !opts.NoCache {
		if b, err := os.ReadFile(path); err == nil {
			return b, nil
		}
	}

	v, err, _ := h.sfg.Do(path, func() (any, error) {
		if !opts.NoCache {
			if b, err := os.ReadFile(path); err == nil {
				return b, nil
			}
		}
		return h.generateStaticMap(ctx, path, basePath, staticMap)
	})
	if err != nil {
		return nil, err
	}
	finalBytes, _ := v.([]byte)

	// TTL tracking: only the final path is owned by this caller.
	// basePath is a shared resource governed by CacheCleaner age eviction.
	if !opts.NoCache && opts.TTL > 0 && services.GlobalExpiryQueue != nil {
		cleanupIndex := func() {
			if services.GlobalCacheIndex != nil {
				services.GlobalCacheIndex.RemoveStaticMap(path)
			}
		}
		services.GlobalExpiryQueue.Add(opts.TTL, cleanupIndex, path)
	}
	return finalBytes, nil
}
```

- [ ] **Step 6: Remove `baseExists` plumbing from the old singleflight section in `handleRequest` (already done in Step 4) and drop the now-unused `baseExists` bool paths.**

- [ ] **Step 7: Serve from memory when possible in `handleRequest`**

After the singleflight block, replace `h.generateResponse(w, r, staticMap, path)` (success path, line ~313) with a helper that prefers in-memory bytes:

```go
h.generateResponseBytes(w, r, staticMap, path, finalBytes)
```

Add:

```go
func (h *StaticMapHandler) generateResponseBytes(w http.ResponseWriter, r *http.Request, staticMap models.StaticMap, path string, finalBytes []byte) {
	if handlePregenerateResponse(w, r, path, staticMap) {
		return
	}
	if finalBytes != nil {
		// Serve directly from memory to close the race window between
		// atomic-write and serve: external deletion of path does not
		// affect this response.
		w.Header().Set("Content-Type", contentTypeFor(staticMap.Format))
		w.Header().Set("Content-Length", strconv.Itoa(len(finalBytes)))
		_, _ = w.Write(finalBytes)
		return
	}
	serveFile(w, r, path)
}
```

Add `contentTypeFor(format)` in `rampardos/internal/handlers/common.go`:

```go
func contentTypeFor(format models.ImageFormat) string {
	switch format {
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		return "image/jpeg"
	case models.ImageFormatWEBP:
		return "image/webp"
	default:
		return "image/png"
	}
}
```

- [ ] **Step 8: Remove `basePath` from TTL expiry queue in `handleRequest`**

In `handleRequest`, the existing block:

```go
if path != basePath {
	services.GlobalExpiryQueue.Add(ttl, cleanupIndex, path, basePath)
} else {
	services.GlobalExpiryQueue.Add(ttl, cleanupIndex, path)
}
```

becomes:

```go
services.GlobalExpiryQueue.Add(ttl, cleanupIndex, path)
```

(The base is shared state; let `CacheCleaner`'s age-based sweep own its lifetime.)

- [ ] **Step 9: Build + test**

```bash
cd rampardos && go build ./... && go test -race ./...
```

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add rampardos/internal/handlers/static_map.go rampardos/internal/handlers/common.go
git commit -m "refactor(handlers): bytes-first single static map flow

- Read basePath into memory once; on miss, render in-memory and
  best-effort persist.
- Deduplicate concurrent base renders via a second singleflight
  keyed on basePath.
- Drop GlobalCacheIndex as the source of truth for base existence
  (stale entries caused false positives leading to read-after-delete).
- Serve final image from in-memory bytes when available.
- Remove basePath from the per-request ExpiryQueue; the base is a
  shared resource owned by CacheCleaner age eviction."
```

---

## Task 4b: `downloadMarkerBytes` — markers held in memory

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go` (replace `downloadMarkers` semantics)
- Modify: `rampardos/internal/utils/image_utils_native.go` (update `drawOverlays` to use `markerBytes` when provided)
- Modify: `rampardos/internal/utils/image_utils.go` (export `GetMarkerPath` / `GetFallbackMarkerPath` if not already; used by handler)

**Context:** `downloadMarkers` currently writes marker PNGs to `Cache/Marker/<hash>.ext` and the overlay draw re-reads them. A `CacheCleaner` sweep on `Cache/Marker` between download and draw causes a hard failure with no retry. This task holds the marker bytes in memory through the draw, closing that window.

- [ ] **Step 1: Export marker path helpers for handler use**

In `rampardos/internal/utils/image_utils.go`, export the existing `getMarkerPath` / `getFallbackMarkerPath` as `GetMarkerPath` / `GetFallbackMarkerPath` (same bodies). Update the sole internal caller in `image_utils_native.go`. Run:

```bash
grep -rn "getMarkerPath\|getFallbackMarkerPath" rampardos/internal/
```

Expected: only the renamed identifiers remain.

- [ ] **Step 2: Write failing test for `downloadMarkerBytes`**

Add to a new or existing test file `rampardos/internal/handlers/marker_bytes_test.go`:

```go
package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/utils"
)

func TestDownloadMarkerBytes_ServesFromMemory(t *testing.T) {
	// Serve a 1x1 PNG from an httptest server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fakePNG(t))
	}))
	defer srv.Close()

	h, cleanup := newTestStaticMapHandler(t)
	defer cleanup()

	sm := models.StaticMap{
		Markers: []models.Marker{{URL: srv.URL + "/a.png", Latitude: 0, Longitude: 0}},
	}

	got, err := h.downloadMarkerBytes(context.Background(), sm)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	key := utils.GetMarkerPath(sm.Markers[0])
	if len(got[key]) == 0 {
		t.Fatalf("expected bytes for %s; got map %v", key, got)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

```bash
cd rampardos && go test ./internal/handlers/ -run TestDownloadMarkerBytes_ServesFromMemory -v
```

Expected: FAIL (undefined `downloadMarkerBytes`).

- [ ] **Step 4: Implement `downloadMarkerBytes`**

In `rampardos/internal/handlers/static_map.go`, replace `downloadMarkers` with:

```go
// downloadMarkerBytes fetches each marker (and fallback, if present) and
// returns a map keyed by the marker's cache path. If the marker is
// already cached on disk, its bytes are read once and reused. If the
// marker must be downloaded, it is written atomically to Cache/Marker
// and the bytes are retained in the returned map.
//
// Callers pass the map to utils.GenerateStaticMapBytes so the overlay
// draw never touches Cache/Marker — closing the race between
// CacheCleaner and draw.
func (h *StaticMapHandler) downloadMarkerBytes(ctx context.Context, staticMap models.StaticMap) (map[string][]byte, error) {
	if len(staticMap.Markers) == 0 {
		return nil, nil
	}
	out := make(map[string][]byte, len(staticMap.Markers))
	for _, m := range staticMap.Markers {
		key := utils.GetMarkerPath(m)
		if _, ok := out[key]; ok {
			continue // same URL reused across markers
		}
		b, err := h.fetchMarkerBytes(ctx, m.URL, key)
		if err != nil && m.FallbackURL != "" {
			fbKey := utils.GetFallbackMarkerPath(m)
			if fb, fbErr := h.fetchMarkerBytes(ctx, m.FallbackURL, fbKey); fbErr == nil {
				out[key] = fb // fallback bytes served under primary key
				continue
			}
		}
		if err != nil {
			return nil, fmt.Errorf("marker %s: %w", m.URL, err)
		}
		out[key] = b
	}
	return out, nil
}

// fetchMarkerBytes returns bytes for a remote marker URL, caching to
// diskPath atomically when missing. For non-URL markers (bundled
// Markers/* files), diskPath is read directly.
func (h *StaticMapHandler) fetchMarkerBytes(ctx context.Context, url, diskPath string) ([]byte, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		// Bundled marker — read from disk (assumed stable, not in Cache/Marker).
		return os.ReadFile(diskPath)
	}
	if b, err := os.ReadFile(diskPath); err == nil {
		return b, nil
	}
	b, err := services.GlobalHTTPService.GetBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := utils.SaveImageBytes(diskPath, b); err != nil {
		slog.Warn("Failed to cache marker", "path", diskPath, "error", err)
	}
	return b, nil
}
```

If `services.GlobalHTTPService.GetBytes` does not exist, add it as a thin wrapper around the existing downloader:

```go
// GetBytes fetches a URL and returns the body as bytes (no disk writes).
func (s *HTTPService) GetBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
```

- [ ] **Step 5: Update `drawOverlays` to consume `markerBytes`**

In `rampardos/internal/utils/image_utils_native.go`, inside the marker draw loop, replace:

```go
markerImg, err := loadImage(utils.GetMarkerPath(marker))
```

with:

```go
var markerImg image.Image
if b, ok := markerBytes[GetMarkerPath(marker)]; ok {
	markerImg, err = decodeImage(b)
} else {
	// Legacy path: no markerBytes provided, fall back to disk.
	markerImg, err = loadImage(GetMarkerPath(marker))
}
```

Apply the same pattern to any fallback-marker branch.

- [ ] **Step 6: Delete the old `downloadMarkers` method**

It's no longer called. Remove the function entirely. `grep` to confirm no remaining references:

```bash
grep -rn "\.downloadMarkers\b" rampardos/internal/
```

Expected: no hits.

- [ ] **Step 7: Build + race test**

```bash
cd rampardos && go build ./... && go test -race ./...
```

Expected: PASS.

- [ ] **Step 8: Race test — marker deleted between download and draw**

Add to `rampardos/internal/handlers/marker_bytes_test.go`:

```go
func TestMarkerDeletedBetweenDownloadAndDrawDoesNotFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fakePNG(t))
	}))
	defer srv.Close()

	h, cleanup := newTestStaticMapHandler(t)
	defer cleanup()

	sm := testStaticMap("m")
	sm.Markers = []models.Marker{{URL: srv.URL + "/a.png", Latitude: sm.Latitude, Longitude: sm.Longitude}}

	markers, err := h.downloadMarkerBytes(context.Background(), sm)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	// Simulate CacheCleaner nuking Cache/Marker before the draw.
	key := utils.GetMarkerPath(sm.Markers[0])
	_ = os.Remove(key)

	// Render must still succeed because markers are in memory.
	baseBytes := fakePNG(t)
	if _, err := utils.GenerateStaticMapBytes(sm, baseBytes, markers, utils.NewSphericalMercator()); err != nil {
		t.Fatalf("render after marker deletion: %v", err)
	}
}
```

- [ ] **Step 9: Run the race test**

```bash
cd rampardos && go test -race ./internal/handlers/ -run TestMarkerDeletedBetweenDownloadAndDrawDoesNotFail -v
```

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add rampardos/internal/handlers/static_map.go rampardos/internal/handlers/marker_bytes_test.go rampardos/internal/utils/image_utils.go rampardos/internal/utils/image_utils_native.go rampardos/internal/services/http.go
git commit -m "feat(handlers): hold marker bytes in memory through overlay draw

Replaces downloadMarkers with downloadMarkerBytes, which returns a
map[string][]byte keyed by marker cache path. The overlay draw reads
from the map instead of disk, closing the CacheCleaner-vs-draw race
window that previously caused hard failures with no retry."
```

---

## Task 5: Race test — external deletion during render

**Files:**
- Create: `rampardos/internal/handlers/static_map_race_test.go`

- [ ] **Step 1: Write failing test (fails *before* the new behaviour landed, passes after)**

```go
package handlers

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
)

// TestBaseDeletedBetweenCallsDoesNotError verifies that if the base
// image file is deleted externally between two GenerateStaticMap
// calls for the same basePath (but different paths), the second call
// still succeeds because we no longer trust a stale cache-index entry.
func TestBaseDeletedBetweenCallsDoesNotError(t *testing.T) {
	h, cleanup := newTestStaticMapHandler(t)
	defer cleanup()

	smA := testStaticMap("a")
	smB := testStaticMap("b") // same coords/zoom/style → same basePath, different overlay markers → different path

	ctx := context.Background()

	if _, err := h.GenerateStaticMap(ctx, smA, GenerateOpts{}); err != nil {
		t.Fatalf("first generate: %v", err)
	}

	// External deletion of the shared base.
	if err := os.Remove(smA.BasePath()); err != nil {
		t.Fatalf("remove base: %v", err)
	}

	if _, err := h.GenerateStaticMap(ctx, smB, GenerateOpts{}); err != nil {
		t.Fatalf("second generate after base delete: %v", err)
	}

	// Final file for B should exist.
	if _, err := os.Stat(smB.Path()); err != nil {
		t.Fatalf("expected final path for B: %v", err)
	}
}

// TestConcurrentSiblingBaseSingleflight verifies that N concurrent
// requests for different paths but the same basePath render the base
// exactly once.
func TestConcurrentSiblingBaseSingleflight(t *testing.T) {
	h, cleanup := newTestStaticMapHandler(t)
	defer cleanup()

	var renders int
	h.generateBaseStaticMapFromTilesFn = func(ctx context.Context, sm models.StaticMap, _ *models.Style) ([]byte, error) {
		renders++
		return fakePNG(t), nil
	}

	const N = 8
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sm := testStaticMap(string(rune('a' + i))) // different paths
			if _, err := h.GenerateStaticMap(context.Background(), sm, GenerateOpts{}); err != nil {
				t.Errorf("generate %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if renders != 1 {
		t.Fatalf("expected 1 base render, got %d", renders)
	}
}
```

Helpers needed: `newTestStaticMapHandler(t)` (constructs a handler with fake renderer + tmp dirs), `testStaticMap(tag)` (builds a `models.StaticMap` with stable coords + a marker whose URL contains `tag` so `Path()` varies), `fakePNG(t)` (returns a valid 8x8 PNG).

Place these in a shared `static_map_testhelpers_test.go` file if not already present.

- [ ] **Step 2: Run tests — they should PASS**

```bash
cd rampardos && go test -race ./internal/handlers/ -run TestBaseDeletedBetweenCallsDoesNotError -v
cd rampardos && go test -race ./internal/handlers/ -run TestConcurrentSiblingBaseSingleflight -v
```

Expected: PASS (because Task 4 landed the bytes-first behaviour and basePath singleflight).

- [ ] **Step 3: Sanity — run against pre-Task-4 commit to confirm the test is meaningful**

```bash
git stash
git checkout HEAD~1 -- rampardos/internal/handlers/static_map.go
cd rampardos && go test ./internal/handlers/ -run TestBaseDeletedBetweenCallsDoesNotError
# Expected: FAIL (stale index hit)
git checkout HEAD -- rampardos/internal/handlers/static_map.go
git stash pop
```

(If the fail mode doesn't reproduce due to `CacheIndex` init ordering in the test harness, it's enough that the test passes post-fix. Skip if painful.)

- [ ] **Step 4: Commit**

```bash
git add rampardos/internal/handlers/static_map_race_test.go rampardos/internal/handlers/static_map_testhelpers_test.go
git commit -m "test(handlers): race tests for external base deletion and sibling singleflight"
```

---

## Task 6: `utils.ComposeMultiStaticMapBytes` — stitch from `[][]byte`

**Files:**
- Modify: `rampardos/internal/utils/image_utils.go` (add wrapper)
- Modify: `rampardos/internal/utils/image_utils_native.go` (add impl, refactor `GenerateMultiStaticMapNative`)

- [ ] **Step 1: Write failing test**

Append to `rampardos/internal/utils/image_bytes_test.go`:

```go
func TestComposeMultiStaticMapBytes_TwoHorizontal(t *testing.T) {
	red := imageBytes(t, color.RGBA{R: 255, A: 255})
	blue := imageBytes(t, color.RGBA{B: 255, A: 255})

	msm := models.MultiStaticMap{
		Grid: []models.MultiStaticMapGrid{
			{
				Direction: models.CombineDirectionFirst,
				Maps: []models.MultiStaticMapItem{
					{Map: models.StaticMap{}, Direction: models.CombineDirectionFirst},
					{Map: models.StaticMap{}, Direction: models.CombineDirectionRight},
				},
			},
		},
	}

	out, err := ComposeMultiStaticMapBytes(msm, [][]byte{red, blue})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	got, err := decodeImage(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got.Bounds().Dx() != 16 || got.Bounds().Dy() != 8 {
		t.Fatalf("bounds: got %v want 16x8", got.Bounds())
	}
}

func imageBytes(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	b, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}
```

Add missing imports (`image/color`, `image/draw`).

- [ ] **Step 2: Run test to verify it fails**

```bash
cd rampardos && go test ./internal/utils/ -run TestComposeMultiStaticMapBytes_TwoHorizontal -v
```

Expected: FAIL (undefined).

- [ ] **Step 3: Add exported wrapper**

In `image_utils.go`:

```go
// ComposeMultiStaticMapBytes composes pre-encoded component images
// into the final grid image. componentBytes must be in grid iteration
// order (outer: grids, inner: maps within each grid, flattened).
func ComposeMultiStaticMapBytes(multiStaticMap models.MultiStaticMap, componentBytes [][]byte) ([]byte, error) {
	return composeMultiStaticMapBytesNative(multiStaticMap, componentBytes)
}
```

- [ ] **Step 4: Add native impl; refactor `GenerateMultiStaticMapNative` to shim**

In `image_utils_native.go`, extract the existing body of `GenerateMultiStaticMapNative` into `composeMultiStaticMapBytesNative` that takes `componentBytes [][]byte`, replacing each `loadImage(mapPath)` with `decodeImage(componentBytes[k])` (indexed by flat order). Final `saveImage(path, result)` becomes `encodeImage(result, models.ImageFormatPNG)`.

Rewrite `GenerateMultiStaticMapNative` as:

```go
func GenerateMultiStaticMapNative(multiStaticMap models.MultiStaticMap, path string) error {
	// Legacy file-path entrypoint: read components from disk, compose, save.
	var componentBytes [][]byte
	for _, grid := range multiStaticMap.Grid {
		for _, m := range grid.Maps {
			b, err := os.ReadFile(m.Map.Path())
			if err != nil {
				return fmt.Errorf("failed to load map %s: %w", m.Map.Path(), err)
			}
			componentBytes = append(componentBytes, b)
		}
	}
	out, err := composeMultiStaticMapBytesNative(multiStaticMap, componentBytes)
	if err != nil {
		return err
	}
	return saveImageBytes(path, out)
}
```

- [ ] **Step 5: Run util tests**

```bash
cd rampardos && go test ./internal/utils/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add rampardos/internal/utils/image_utils.go rampardos/internal/utils/image_utils_native.go rampardos/internal/utils/image_bytes_test.go
git commit -m "feat(utils): bytes-first ComposeMultiStaticMapBytes"
```

---

## Task 7: Multi-static handler — parallel bytes fetch + in-memory compose

**Files:**
- Modify: `rampardos/internal/handlers/multi_static_map.go`

- [ ] **Step 1: Rewrite the generate+combine section in `handleRequest`**

Replace the `h.sfg.Do(path, ...)` block (lines ~180-229 in current `multi_static_map.go`) with:

```go
v, sfErr, _ := h.sfg.Do(path, func() (any, error) {
	if !skipCache {
		if b, err := os.ReadFile(path); err == nil {
			return b, nil
		}
	}

	// Flatten component order.
	var components []models.StaticMap
	for _, grid := range multiStaticMap.Grid {
		for _, m := range grid.Maps {
			components = append(components, m.Map)
		}
	}

	// Component opts flow-through: nocache/ttl apply to cacheable
	// components. With skipCache, components return bytes and are not
	// persisted to disk at all.
	componentOpts := GenerateOpts{NoCache: skipCache}
	if ttlSeconds > 0 {
		componentOpts.TTL = time.Duration(ttlSeconds) * time.Second
	}

	// Parallel component generation, preserving slot order for the stitcher.
	componentBytes := make([][]byte, len(components))
	errCh := make(chan error, len(components))
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i, sm := range components {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, sm models.StaticMap) {
			defer wg.Done()
			defer func() { <-sem }()
			b, err := h.staticMapHandler.GenerateStaticMap(r.Context(), sm, componentOpts)
			if err != nil {
				errCh <- err
				return
			}
			componentBytes[i] = b
		}(i, sm)
	}
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return nil, err
	}

	stitched, err := utils.ComposeMultiStaticMapBytes(multiStaticMap, componentBytes)
	if err != nil {
		return nil, err
	}
	if !skipCache {
		if err := utils.SaveImageBytes(path, stitched); err != nil {
			return nil, err
		}
	}
	return stitched, nil
})
```

After the block, update the response path to serve from memory:

```go
if sfErr != nil {
	// ...existing error handling...
}

var stitched []byte
if v != nil {
	stitched, _ = v.([]byte)
}

duration := time.Since(startTime).Seconds()
h.statsController.StaticMapServed(true, path, "multi")
services.GlobalMetrics.RecordRequest("multistaticmap", "multi", false, duration)

if skipCache {
	// nocache: serve bytes directly, never touching disk.
	slog.Debug("Served multi-static map (nocache)", "file", filepath.Base(path), "maps", len(stitched), "duration", duration)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(stitched)))
	_, _ = w.Write(stitched)
	return
}

if services.GlobalCacheIndex != nil {
	services.GlobalCacheIndex.AddMultiStaticMap(path)
}
slog.Debug("Served multi-static map (generated)", "file", filepath.Base(path), "duration", duration, "ttl", ttlSeconds)
h.generateResponseBytes(w, r, multiStaticMap, path, stitched)

if ttlSeconds > 0 && services.GlobalExpiryQueue != nil {
	cleanupIndex := func() {
		if services.GlobalCacheIndex != nil {
			services.GlobalCacheIndex.RemoveMultiStaticMap(path)
		}
	}
	services.GlobalExpiryQueue.Add(time.Duration(ttlSeconds)*time.Second, cleanupIndex, path)
}
```

Add `generateResponseBytes` on `MultiStaticMapHandler` mirroring the single-map helper:

```go
func (h *MultiStaticMapHandler) generateResponseBytes(w http.ResponseWriter, r *http.Request, msm models.MultiStaticMap, path string, stitched []byte) {
	if handlePregenerateResponse(w, r, path, msm) {
		return
	}
	if stitched != nil {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(stitched)))
		_, _ = w.Write(stitched)
		return
	}
	serveFile(w, r, path)
}
```

- [ ] **Step 2: Remove the old component-file-cleanup block**

The existing loop:

```go
if skipCache {
	for _, sm := range mapsToGenerate {
		os.Remove(sm.Path())
		bp := sm.BasePath()
		if bp != sm.Path() {
			os.Remove(bp)
		}
	}
}
```

is no longer needed — with `componentOpts.NoCache = true`, components never hit disk. Delete it.

- [ ] **Step 3: Build + test**

```bash
cd rampardos && go build ./... && go test -race ./...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add rampardos/internal/handlers/multi_static_map.go
git commit -m "refactor(handlers): bytes-first multi static map flow

Components are fetched as [][]byte in parallel, composed in memory,
and written atomically. Eliminates the read-after-delete race where
a TTL sweep could remove a component file between GenerateStaticMap
and the stitcher's load. With nocache, components never touch disk."
```

---

## Task 8: `GenerateStaticMap(ctx, sm, opts) ([]byte, error)` — support nocache bytes path

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go` (already touched in Task 4, but now we need the explicit nocache bytes path for multi components)

- [ ] **Step 1: Add nocache branch in `GenerateStaticMap`**

Ensure `GenerateStaticMap` (from Task 4) honours `opts.NoCache` by **not** writing to disk for the no-drawable case. Update `generateStaticMap` (unexported) to accept an explicit "persist?" argument:

```go
func (h *StaticMapHandler) generateStaticMap(ctx context.Context, path, basePath string, staticMap models.StaticMap, persist bool) ([]byte, error) {
	// ...as Task 4, but gate every SaveImageBytes call on `persist`...
}
```

Update both call sites (`handleRequest` passes `persist: true`; `GenerateStaticMap` passes `persist: !opts.NoCache`).

- [ ] **Step 2: Build + test**

```bash
cd rampardos && go test -race ./...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add rampardos/internal/handlers/static_map.go
git commit -m "feat(handlers): GenerateStaticMap honours NoCache by not persisting"
```

---

## Task 9: Multi-map race test

**Files:**
- Modify: `rampardos/internal/handlers/static_map_race_test.go` (append) or new file.

- [ ] **Step 1: Add test**

```go
// TestMultiStaticMapComponentDeletedMidFlight verifies the stitcher
// composes from in-memory bytes and is not affected by concurrent
// deletion of component files.
func TestMultiStaticMapComponentDeletedMidFlight(t *testing.T) {
	sh, mh, cleanup := newTestMultiStaticMapHandler(t)
	defer cleanup()
	_ = sh

	msm := testMultiStaticMap()
	// Pre-create the components on disk.
	for _, grid := range msm.Grid {
		for _, m := range grid.Maps {
			if err := os.MkdirAll(filepath.Dir(m.Map.Path()), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.Map.Path(), fakePNG(t), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/multistaticmap", bytes.NewReader(mustJSON(msm)))

	// Delete components in a goroutine during the stitch.
	go func() {
		time.Sleep(5 * time.Millisecond)
		for _, grid := range msm.Grid {
			for _, m := range grid.Maps {
				os.Remove(m.Map.Path())
			}
		}
	}()

	mh.Post(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d body=%q", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: Run test**

```bash
cd rampardos && go test -race ./internal/handlers/ -run TestMultiStaticMapComponentDeletedMidFlight -v
```

Expected: PASS (components bytes are in memory before the stitch).

- [ ] **Step 3: Commit**

```bash
git add rampardos/internal/handlers/static_map_race_test.go
git commit -m "test(handlers): multi-static map survives component deletion mid-flight"
```

---

## Task 10: Cleanup — delete dead code and update docs

**Files:**
- Modify: `rampardos/internal/handlers/static_map.go`
- Modify: `rampardos/internal/handlers/multi_static_map.go`
- Modify: `rampardos/internal/services/cache_index.go` (only if a public method becomes unused)

- [ ] **Step 1: Search for dead references to `baseExists` / `HasStaticMap(basePath)`**

```bash
grep -rn "baseExists\|HasStaticMap(basePath\|AddStaticMap(basePath" rampardos/internal/
```

Expected: no hits after the refactor. If anything remains, delete it.

- [ ] **Step 2: Verify `ExpiryQueue.Add(..., path, basePath)` is gone**

```bash
grep -rn "ExpiryQueue.Add.*basePath" rampardos/internal/
```

Expected: no hits.

- [ ] **Step 3: Run full suite + race**

```bash
cd rampardos && go test -race ./...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor: remove dead base-existence cache-index paths"
```

---

## Task 11: Manual smoke test + push

**Files:** none.

- [ ] **Step 1: Build Docker image**

```bash
docker build -t rampardos:bytes-first .
```

Expected: build succeeds.

- [ ] **Step 2: Run and hit staticmap with overlays**

```bash
docker run --rm -p 8080:8080 \
  -e DEBUG=true \
  -v $(pwd)/TileServer:/app/TileServer \
  rampardos:bytes-first &

curl -sS -X POST http://localhost:8080/staticmap \
  -H 'Content-Type: application/json' \
  -d '{"latitude":51.5,"longitude":0,"zoom":12,"width":400,"height":400,"style":"klokantech-basic","markers":[{"url":"https://example.com/marker.png","latitude":51.5,"longitude":0}]}' \
  --output /tmp/smoke.png

file /tmp/smoke.png
```

Expected: `PNG image data`.

- [ ] **Step 3: Delete base file mid-stream — ensure second request succeeds**

```bash
# First call populates base
curl -sS ... > /tmp/first.png
# Remove the base file (name is deterministic — find in Cache/Static)
rm "TileServer/.../Cache/Static/<basehash>.png"
# Second call with different markers → different path, shared basePath
curl -sS ... > /tmp/second.png
file /tmp/second.png
```

Expected: `PNG image data`, no 500.

- [ ] **Step 4: Push branch + open PR**

```bash
git push -u upstream fix-race-bytes-first
gh pr create --repo lenisko/rampardos --base tileserver-replacement --title "fix: race-free static map generation via bytes-first pipeline" --body "..."
```

---

## Self-Review Notes

**Spec coverage check:**
- ✅ Base read once into `[]byte` → Task 4 Step 2 (`loadOrRenderBaseBytes`).
- ✅ On ENOENT, render in-memory, best-effort persist → Task 4 Step 2.
- ✅ Draw overlays onto bytes → Task 2.
- ✅ Atomic write final → Task 2 (`SaveImageBytes`) called from Task 4.
- ✅ Multi compose from `[][]byte` → Task 6.
- ✅ Remove `basePath` from expiry queue → Task 4 Step 8.
- ✅ Cache index only for final-path hints → Task 4 Step 4.
- ✅ Survive external deletion of base → Tasks 5, 9.
- ✅ Per-basePath singleflight for sibling concurrency → Task 4 Step 1.
- ✅ Serve final bytes from memory (close last race) → Task 4 Step 7.
- ✅ Markers held in memory through draw (closes CacheCleaner-vs-draw race) → Task 4b.
- ⚠️ Tile files remain on the disk-read path; mitigated by fresh mtime + `loadImageWithRetry` redownload callback. Out of scope per plan's Scope note.

**Placeholder scan:** No TBDs, no "implement later", no "similar to Task N" references. All code shown inline.

**Type consistency:**
- `GenerateStaticMapBytes(sm, baseBytes, markerBytes, merc) ([]byte, error)` — signature consistent across Tasks 2, 4, 4b, 7.
- `downloadMarkerBytes(ctx, sm) (map[string][]byte, error)` — Task 4b; called from `generateStaticMap` in Task 4.
- `drawOverlays(dc, sm, scale, markerBytes, merc) error` — Task 2 (extracted helper), Task 4b (marker-bytes consumer).
- `GetMarkerPath` / `GetFallbackMarkerPath` — exported in Task 4b Step 1.
- `generateStaticMap(ctx, path, basePath, sm, persist) ([]byte, error)` — defined in Task 4, refined in Task 8 (added `persist bool`). Call sites updated in Task 8 Step 1.
- `GenerateStaticMap(ctx, sm, opts) ([]byte, error)` — Task 4, Task 8. Called by multi handler in Task 7.
- `ComposeMultiStaticMapBytes(msm, [][]byte) ([]byte, error)` — Task 6; called in Task 7.
- `SaveImageBytes` — exported in Task 2; called from Task 4, 6, 7.
- `loadOrRenderBaseBytes(ctx, sm, basePath) ([]byte, error)` — Task 4; called from `generateStaticMap`.
- `h.baseSfg` — added in Task 4 Step 1; used in `loadOrRenderBaseBytes`.

All consistent.

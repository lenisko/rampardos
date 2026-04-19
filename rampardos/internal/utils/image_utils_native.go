package utils

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// pngBufferPool reuses png.EncoderBuffer across encodes so the
// internal zlib writer and filter working buffers don't allocate
// fresh per call. Encoder instances themselves are lightweight
// (just CompressionLevel + a pointer); the pool is what matters.
var pngBufferPool pngEncoderBufferPool

type pngEncoderBufferPool struct {
	pool sync.Pool
}

func (p *pngEncoderBufferPool) Get() *png.EncoderBuffer {
	if b, ok := p.pool.Get().(*png.EncoderBuffer); ok {
		return b
	}
	return nil
}

func (p *pngEncoderBufferPool) Put(buf *png.EncoderBuffer) {
	p.pool.Put(buf)
}

// GenerateBaseStaticMapNative combines tiles into a base static map and
// returns the cropped image. The caller owns persistence — in the
// bytes-first pipeline the stitched base lives only in the composite
// image LRU, not on disk.
func GenerateBaseStaticMapNative(staticMap models.StaticMap, tilePaths []string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) (image.Image, error) {
	if len(tilePaths) == 0 {
		return nil, fmt.Errorf("no tiles to combine")
	}

	// Sort tile paths for consistent ordering
	sortedPaths := make([]string, len(tilePaths))
	copy(sortedPaths, tilePaths)
	sort.Strings(sortedPaths)

	// Load all tiles and determine grid dimensions
	tiles := make([]image.Image, 0, len(sortedPaths))
	var tileWidth, tileHeight int

	for _, tilePath := range sortedPaths {
		img, err := loadImageWithRetry(tilePath, redownload)
		if err != nil {
			return nil, fmt.Errorf("failed to load tile %s: %w", tilePath, err)
		}
		tiles = append(tiles, img)
		if tileWidth == 0 {
			tileWidth = img.Bounds().Dx()
			tileHeight = img.Bounds().Dy()
		}
	}

	// Determine grid layout from tile paths
	// Tiles are named like: Cache/Tile/style-z-x-y-scale.format
	type tilePos struct {
		x, y int
		img  image.Image
	}
	var positions []tilePos
	minX, minY := int(^uint(0)>>1), int(^uint(0)>>1)
	maxX, maxY := 0, 0

	for i, tilePath := range sortedPaths {
		split := strings.Split(tilePath, "-")
		if len(split) < 5 {
			continue
		}
		// Format: style-z-x-y-scale.format
		// split[len-1] = scale.format
		// split[len-2] = y
		// split[len-3] = x
		x, _ := strconv.Atoi(split[len(split)-3])
		y, _ := strconv.Atoi(split[len(split)-2])

		positions = append(positions, tilePos{x, y, tiles[i]})
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}

	// Create combined image
	gridWidth := (maxX - minX + 1) * tileWidth
	gridHeight := (maxY - minY + 1) * tileHeight
	combined := image.NewNRGBA(image.Rect(0, 0, gridWidth, gridHeight))

	// Draw tiles at their positions
	for _, pos := range positions {
		drawX := (pos.x - minX) * tileWidth
		drawY := (pos.y - minY) * tileHeight
		draw.Draw(combined, image.Rect(drawX, drawY, drawX+tileWidth, drawY+tileHeight),
			pos.img, image.Point{}, draw.Src)
	}

	// Calculate crop dimensions
	var imgWidth, imgHeight, imgWidthOffset, imgHeightOffset int
	if hasScale && staticMap.Scale > 1 {
		imgWidth = int(staticMap.Width) * int(staticMap.Scale)
		imgHeight = int(staticMap.Height) * int(staticMap.Scale)
		imgWidthOffset = (offsetX - int(staticMap.Width)/2) * int(staticMap.Scale)
		imgHeightOffset = (offsetY - int(staticMap.Height)/2) * int(staticMap.Scale)
	} else {
		imgWidth = int(staticMap.Width)
		imgHeight = int(staticMap.Height)
		imgWidthOffset = offsetX - int(staticMap.Width)/2
		imgHeightOffset = offsetY - int(staticMap.Height)/2
	}

	// Crop to final size
	cropped := image.NewNRGBA(image.Rect(0, 0, imgWidth, imgHeight))
	draw.Draw(cropped, cropped.Bounds(), combined,
		image.Point{X: imgWidthOffset, Y: imgHeightOffset}, draw.Src)

	return cropped, nil
}

// drawPolygon draws a single polygon (fill + optional stroke) onto dc.
func drawPolygon(dc *gg.Context, staticMap models.StaticMap, polygon models.Polygon, sm *SphericalMercator, scale uint8) error {
	// Set fill color
	fillColor := parseColor(polygon.FillColor)
	dc.SetColor(fillColor)

	// Draw polygon path
	for i, coord := range polygon.Path {
		if len(coord) != 2 {
			return fmt.Errorf("expecting two values to form a coordinate but got %d", len(coord))
		}
		point := getRealOffset(
			models.Coordinate{Latitude: coord[0], Longitude: coord[1]},
			models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
			staticMap.Zoom, staticMap.Scale, 0, 0, sm,
		)
		x := float64(point.x + int(staticMap.Width/2*uint16(scale)))
		y := float64(point.y + int(staticMap.Height/2*uint16(scale)))

		if i == 0 {
			dc.MoveTo(x, y)
		} else {
			dc.LineTo(x, y)
		}
	}
	dc.ClosePath()
	dc.Fill()

	// Draw stroke
	if polygon.StrokeWidth > 0 {
		strokeColor := parseColor(polygon.StrokeColor)
		dc.SetColor(strokeColor)
		dc.SetLineWidth(float64(polygon.StrokeWidth))

		for i, coord := range polygon.Path {
			point := getRealOffset(
				models.Coordinate{Latitude: coord[0], Longitude: coord[1]},
				models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
				staticMap.Zoom, staticMap.Scale, 0, 0, sm,
			)
			x := float64(point.x + int(staticMap.Width/2*uint16(scale)))
			y := float64(point.y + int(staticMap.Height/2*uint16(scale)))

			if i == 0 {
				dc.MoveTo(x, y)
			} else {
				dc.LineTo(x, y)
			}
		}
		dc.ClosePath()
		dc.Stroke()
	}
	return nil
}

// drawCircle draws a single circle (fill + optional stroke) onto dc.
func drawCircle(dc *gg.Context, staticMap models.StaticMap, circle models.Circle, sm *SphericalMercator, scale uint8) {
	coord := models.Coordinate{Latitude: circle.Latitude, Longitude: circle.Longitude}
	point := getRealOffset(
		coord,
		models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
		staticMap.Zoom, staticMap.Scale, 0, 0, sm,
	)
	radiusCoord := coord.CoordinateAt(circle.Radius, 0)
	radius := float64(getRealOffset(coord, radiusCoord, staticMap.Zoom, staticMap.Scale, 0, 0, sm).y)

	x := float64(point.x + int(staticMap.Width)*int(scale)/2)
	y := float64(point.y + int(staticMap.Height)*int(scale)/2)

	// Fill circle
	fillColor := parseColor(circle.FillColor)
	dc.SetColor(fillColor)
	dc.DrawCircle(x, y, radius)
	dc.Fill()

	// Stroke circle
	if circle.StrokeWidth > 0 {
		strokeColor := parseColor(circle.StrokeColor)
		dc.SetColor(strokeColor)
		dc.SetLineWidth(float64(circle.StrokeWidth))
		dc.DrawCircle(x, y, radius)
		dc.Stroke()
	}
}

// GenerateStaticMapFromImage renders overlays on top of baseImg and
// returns the resulting image.Image. Unlike GenerateStaticMapNative
// it neither reads from nor writes to disk — the caller owns
// persistence.
//
// Marker-only requests skip gg entirely and stay NRGBA end-to-end.
// When polygons or circles are present we use gg for its anti-aliased
// rasterisers, convert its RGBA canvas to NRGBA, then stamp markers
// on the NRGBA canvas so marker draws hit draw.Draw's same-type path
// instead of xdraw's RGBA→NRGBA transform.
func GenerateStaticMapFromImage(staticMap models.StaticMap, baseImg image.Image, sm *SphericalMercator) (image.Image, error) {
	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	if len(staticMap.Polygons) == 0 && len(staticMap.Circles) == 0 {
		return drawMarkersNative(staticMap, baseImg, sm, scale), nil
	}

	dc := gg.NewContextForImage(baseImg)

	for _, polygon := range staticMap.Polygons {
		if len(polygon.Path) == 0 {
			continue
		}
		if err := drawPolygon(dc, staticMap, polygon, sm, scale); err != nil {
			return nil, err
		}
	}
	for _, circle := range staticMap.Circles {
		drawCircle(dc, staticMap, circle, sm, scale)
	}

	canvas := toNRGBA(dc.Image())
	for _, marker := range staticMap.Markers {
		drawMarkerNRGBA(canvas, staticMap, marker, sm, scale)
	}
	return canvas, nil
}

// drawMarkersNative renders markers onto a fresh NRGBA copy of
// baseImg. Skips gg's RGBA round-trip.
func drawMarkersNative(staticMap models.StaticMap, baseImg image.Image, sm *SphericalMercator, scale uint8) image.Image {
	b := baseImg.Bounds()
	canvas := image.NewNRGBA(b)
	draw.Draw(canvas, b, baseImg, b.Min, draw.Src)

	for _, marker := range staticMap.Markers {
		drawMarkerNRGBA(canvas, staticMap, marker, sm, scale)
	}
	return canvas
}

// drawMarkerNRGBA stamps a single marker onto an NRGBA canvas via
// draw.Draw (NRGBA→NRGBA Over fast path). Skips the marker when
// outside the visible area or when loading fails.
func drawMarkerNRGBA(canvas *image.NRGBA, staticMap models.StaticMap, marker models.Marker, sm *SphericalMercator, scale uint8) {
	realOffset := getRealOffset(
		models.Coordinate{Latitude: marker.Latitude, Longitude: marker.Longitude},
		models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
		staticMap.Zoom, staticMap.Scale, int(marker.XOffset), int(marker.YOffset), sm,
	)

	if abs(realOffset.x) > int(staticMap.Width+marker.Width)*int(scale)/2 ||
		abs(realOffset.y) > int(staticMap.Height+marker.Height)*int(scale)/2 {
		return
	}

	markerPath := getMarkerPath(marker)
	if marker.FallbackURL != "" {
		if _, err := os.Stat(markerPath); os.IsNotExist(err) {
			markerPath = getFallbackMarkerPath(marker)
		}
	}

	targetWidth := int(marker.Width) * int(scale)
	targetHeight := int(marker.Height) * int(scale)

	var markerImg image.Image
	if services.GlobalCacheIndex != nil && targetWidth > 0 && targetHeight > 0 {
		if cached, ok := services.GlobalCacheIndex.GetMarkerImage(markerPath, targetWidth, targetHeight); ok {
			markerImg = cached
		}
	}
	if markerImg == nil {
		var err error
		markerImg, err = loadImage(markerPath)
		if err != nil {
			return
		}
		if targetWidth > 0 && targetHeight > 0 {
			markerImg = resizeImage(markerImg, targetWidth, targetHeight)
			if services.GlobalCacheIndex != nil {
				services.GlobalCacheIndex.AddMarkerImage(markerPath, targetWidth, targetHeight, markerImg)
			}
		}
	}

	centerX := canvas.Bounds().Dx() / 2
	centerY := canvas.Bounds().Dy() / 2
	drawX := centerX + realOffset.x - markerImg.Bounds().Dx()/2
	drawY := centerY + realOffset.y - markerImg.Bounds().Dy()/2

	mb := markerImg.Bounds()
	dstRect := image.Rect(drawX, drawY, drawX+mb.Dx(), drawY+mb.Dy())
	draw.Draw(canvas, dstRect, markerImg, mb.Min, draw.Over)
}

// GenerateStaticMapNative adds markers, polygons, and circles to a base map using native Go
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

// GenerateMultiStaticMapNative combines multiple static maps into a grid using native Go.
//
// This matches ImageMagick's -append/+append behaviour used by the
// original Swift implementation: each image in a grid group is
// sequentially appended to the cumulative result, and images are
// scaled to match the append dimension (height for +append/right,
// width for -append/bottom) so the seams align cleanly.
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

// appendImages combines two images in the given direction, scaling the
// smaller image to match the larger's dimension along the append axis.
// This matches ImageMagick's +append (right) and -append (bottom).
func appendImages(base, addition image.Image, direction models.CombineDirection) image.Image {
	baseW := base.Bounds().Dx()
	baseH := base.Bounds().Dy()
	addW := addition.Bounds().Dx()
	addH := addition.Bounds().Dy()

	switch direction {
	case models.CombineDirectionRight:
		// Horizontal append: scale addition to match base's height.
		targetH := baseH
		if addH != targetH && addH > 0 {
			scaledW := addW * targetH / addH
			addition = scaleImage(addition, scaledW, targetH)
			addW = scaledW
			addH = targetH
		}
		combined := image.NewNRGBA(image.Rect(0, 0, baseW+addW, targetH))
		draw.Draw(combined, image.Rect(0, 0, baseW, baseH), base, image.Point{}, draw.Src)
		draw.Draw(combined, image.Rect(baseW, 0, baseW+addW, addH), addition, image.Point{}, draw.Src)
		return combined

	case models.CombineDirectionBottom:
		// Vertical append: scale addition to match base's width.
		targetW := baseW
		if addW != targetW && addW > 0 {
			scaledH := addH * targetW / addW
			addition = scaleImage(addition, targetW, scaledH)
			addW = targetW
			addH = scaledH
		}
		combined := image.NewNRGBA(image.Rect(0, 0, targetW, baseH+addH))
		draw.Draw(combined, image.Rect(0, 0, baseW, baseH), base, image.Point{}, draw.Src)
		draw.Draw(combined, image.Rect(0, baseH, addW, baseH+addH), addition, image.Point{}, draw.Src)
		return combined

	default:
		// CombineDirectionFirst or unknown — treat as right.
		return appendImages(base, addition, models.CombineDirectionRight)
	}
}

// scaleImage resizes an image to the target dimensions using high-quality
// CatmullRom interpolation.
func scaleImage(src image.Image, width, height int) image.Image {
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

// loadImage loads an image from a file
func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	return img, err
}

// toNRGBA normalises a decoded image to *image.NRGBA so later
// draw.Draw calls into an NRGBA canvas hit the fast same-type copy
// instead of the generic drawMask path. Identity return for images
// already NRGBA (vector-tile PNGs decode to NRGBA natively). Pays
// one draw.Draw on first decode for YCbCr (JPEG, e.g. mapbox
// satellite tiles) and RGBA (no-alpha PNG) sources; the converted
// NRGBA is what the caller caches, so the conversion is amortised
// across every subsequent stitch that hits the LRU.
func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok {
		return n
	}
	b := img.Bounds()
	dst := image.NewNRGBA(b)
	draw.Draw(dst, b, img, b.Min, draw.Src)
	return dst
}

// loadImageWithRetry loads an image, retrying once via redownload if
// corrupted. Consults GlobalTileImageCache first — pprof showed PNG
// decode of already-disk-cached tiles dominating base-map stitching
// at 54% of CPU, and an in-memory LRU eliminates the repeat decode
// since the same tiles are requested heavily across adjacent
// staticmaps. A corrupted-file retry invalidates the cached entry.
func loadImageWithRetry(path string, redownload TileRedownloader) (image.Image, error) {
	if services.GlobalTileImageCache != nil {
		start := time.Now()
		if img, ok := services.GlobalTileImageCache.Get(path); ok {
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.RecordTileDecode(services.TileDecodeSourceRAMLRU, time.Since(start).Seconds())
			}
			return img, nil
		}
	}

	start := time.Now()
	img, err := loadImage(path)
	if err != nil && redownload != nil {
		slog.Warn("Corrupted tile detected, attempting redownload", "path", path, "error", err)
		os.Remove(path)
		if rerr := redownload(path); rerr != nil {
			return nil, fmt.Errorf("redownload failed: %w", rerr)
		}
		img, err = loadImage(path)
		if err != nil {
			return nil, fmt.Errorf("still corrupted after redownload: %w", err)
		}
		slog.Info("Tile redownload successful", "path", path)
	}

	if err == nil && img != nil {
		// Normalise once to NRGBA. Stitch canvases are NRGBA, so
		// caching non-NRGBA (YCbCr from JPEG, RGBA from opaque PNG)
		// would force draw.Draw into the generic drawMask path on
		// every hit. Do the conversion at the LRU boundary instead.
		img = toNRGBA(img)
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordTileDecode(services.TileDecodeSourceDisk, time.Since(start).Seconds())
		}
		if services.GlobalTileImageCache != nil {
			services.GlobalTileImageCache.Add(path, img)
		}
	}
	return img, err
}

// saveImage saves an image to a file atomically. It writes to a
// temporary file then renames, so concurrent readers never see a
// partially-encoded image.
func saveImage(path string, img image.Image) error {
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}

	ext := strings.ToLower(filepath.Ext(path))
	var encodeErr error
	switch ext {
	case ".jpg", ".jpeg":
		quality := 90
		if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
			quality = services.GlobalImageSettings.ImageQuality
		}
		encodeErr = jpeg.Encode(f, img, &jpeg.Options{Quality: quality})
	case ".webp":
		quality := 90
		if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
			quality = services.GlobalImageSettings.ImageQuality
		}
		encodeErr = webp.Encode(f, img, webp.Options{Quality: quality})
	default:
		encoder := &png.Encoder{CompressionLevel: png.BestCompression, BufferPool: &pngBufferPool}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		encodeErr = encoder.Encode(f, img)
	}

	tmpName := f.Name()
	f.Close()
	if encodeErr != nil {
		os.Remove(tmpName)
		return encodeErr
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// resizeImage resizes an image to the target dimensions
func resizeImage(img image.Image, width, height int) image.Image {
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	xdraw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
	return dst
}

// parseColor parses a color string (hex, rgba, or named) into color.Color
func parseColor(s string) color.Color {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" || s == "transparent" {
		return color.Transparent
	}

	// Handle rgba() format: rgba(r,g,b,a) where a is 0-1
	if strings.HasPrefix(strings.ToLower(s), "rgba(") && strings.HasSuffix(s, ")") {
		inner := s[5 : len(s)-1]
		parts := strings.Split(inner, ",")
		if len(parts) == 4 {
			var r, g, b int
			var a float64
			fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &r)
			fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &g)
			fmt.Sscanf(strings.TrimSpace(parts[2]), "%d", &b)
			fmt.Sscanf(strings.TrimSpace(parts[3]), "%f", &a)
			// Use NRGBA (non-premultiplied alpha) for correct transparency
			return color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(a * 255)}
		}
	}

	// Handle rgb() format: rgb(r,g,b)
	if strings.HasPrefix(strings.ToLower(s), "rgb(") && strings.HasSuffix(s, ")") {
		inner := s[4 : len(s)-1]
		parts := strings.Split(inner, ",")
		if len(parts) == 3 {
			var r, g, b int
			fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &r)
			fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &g)
			fmt.Sscanf(strings.TrimSpace(parts[2]), "%d", &b)
			return color.NRGBA{uint8(r), uint8(g), uint8(b), 255}
		}
	}

	// Handle hex colors
	s = strings.TrimPrefix(s, "#")

	var r, g, b, a uint8 = 0, 0, 0, 255

	switch len(s) {
	case 3: // RGB
		fmt.Sscanf(s, "%1x%1x%1x", &r, &g, &b)
		r *= 17
		g *= 17
		b *= 17
	case 4: // RGBA
		fmt.Sscanf(s, "%1x%1x%1x%1x", &r, &g, &b, &a)
		r *= 17
		g *= 17
		b *= 17
		a *= 17
	case 6: // RRGGBB
		fmt.Sscanf(s, "%02x%02x%02x", &r, &g, &b)
	case 8: // RRGGBBAA
		fmt.Sscanf(s, "%02x%02x%02x%02x", &r, &g, &b, &a)
	default:
		// Try named colors
		switch strings.ToLower(s) {
		case "black":
			return color.Black
		case "white":
			return color.White
		case "red":
			return color.NRGBA{255, 0, 0, 255}
		case "green":
			return color.NRGBA{0, 255, 0, 255}
		case "blue":
			return color.NRGBA{0, 0, 255, 255}
		case "yellow":
			return color.NRGBA{255, 255, 0, 255}
		}
	}

	// Use NRGBA for correct alpha handling
	return color.NRGBA{r, g, b, a}
}

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
		encoder := &png.Encoder{CompressionLevel: png.BestSpeed, BufferPool: &pngBufferPool}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

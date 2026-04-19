package utils

import (
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

	"github.com/fogleman/gg"
	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// GenerateBaseStaticMapNative combines tiles into a base static map using native Go
func GenerateBaseStaticMapNative(staticMap models.StaticMap, tilePaths []string, path string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) error {
	if len(tilePaths) == 0 {
		return fmt.Errorf("no tiles to combine")
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
			return fmt.Errorf("failed to load tile %s: %w", tilePath, err)
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

	return saveImage(path, cropped)
}

// GenerateStaticMapNative adds markers, polygons, and circles to a base map using native Go
func GenerateStaticMapNative(staticMap models.StaticMap, basePath, path string, sm *SphericalMercator) error {
	// Load base image
	baseImg, err := loadImage(basePath)
	if err != nil {
		return fmt.Errorf("failed to load base image: %w", err)
	}

	// Create drawing context
	dc := gg.NewContextForImage(baseImg)

	scale := staticMap.Scale
	if scale == 0 {
		scale = 1
	}

	// Process polygons
	for _, polygon := range staticMap.Polygons {
		if len(polygon.Path) == 0 {
			continue
		}

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
	}

	// Process circles
	for _, circle := range staticMap.Circles {
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

	// Process markers
	for _, marker := range staticMap.Markers {
		realOffset := getRealOffset(
			models.Coordinate{Latitude: marker.Latitude, Longitude: marker.Longitude},
			models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
			staticMap.Zoom, staticMap.Scale, int(marker.XOffset), int(marker.YOffset), sm,
		)

		// Skip markers outside the visible area
		if abs(realOffset.x) > int(staticMap.Width+marker.Width)*int(scale)/2 ||
			abs(realOffset.y) > int(staticMap.Height+marker.Height)*int(scale)/2 {
			continue
		}

		markerPath := getMarkerPath(marker)
		if marker.FallbackURL != "" {
			if _, err := os.Stat(markerPath); os.IsNotExist(err) {
				markerPath = getFallbackMarkerPath(marker)
			}
		}

		// Always resize marker to target dimensions (matching ImageMagick behavior)
		targetWidth := int(marker.Width) * int(scale)
		targetHeight := int(marker.Height) * int(scale)

		// Try to get from in-memory cache first
		var markerImg image.Image
		if services.GlobalCacheIndex != nil && targetWidth > 0 && targetHeight > 0 {
			if cached, ok := services.GlobalCacheIndex.GetMarkerImage(markerPath, targetWidth, targetHeight); ok {
				markerImg = cached
			}
		}

		// Load and resize if not cached
		if markerImg == nil {
			var err error
			markerImg, err = loadImage(markerPath)
			if err != nil {
				continue // Skip markers that can't be loaded
			}

			if targetWidth > 0 && targetHeight > 0 {
				markerImg = resizeImage(markerImg, targetWidth, targetHeight)
				// Cache the resized image
				if services.GlobalCacheIndex != nil {
					services.GlobalCacheIndex.AddMarkerImage(markerPath, targetWidth, targetHeight, markerImg)
				}
			}
		}

		// Calculate position (ImageMagick uses -gravity Center, so offset is from center)
		// The marker should be centered at (centerX + offsetX, centerY + offsetY)
		centerX := dc.Width() / 2
		centerY := dc.Height() / 2

		// Draw marker at position (top-left corner calculation)
		drawX := centerX + realOffset.x - markerImg.Bounds().Dx()/2
		drawY := centerY + realOffset.y - markerImg.Bounds().Dy()/2
		dc.DrawImage(markerImg, drawX, drawY)
	}

	return saveImage(path, dc.Image())
}

// GenerateMultiStaticMapNative combines multiple static maps into a grid using native Go.
//
// This matches ImageMagick's -append/+append behaviour used by the
// original Swift implementation: each image in a grid group is
// sequentially appended to the cumulative result, and images are
// scaled to match the append dimension (height for +append/right,
// width for -append/bottom) so the seams align cleanly.
func GenerateMultiStaticMapNative(multiStaticMap models.MultiStaticMap, path string) error {
	// Build each grid group as a composite image, then combine groups.
	var groupImages []image.Image
	var groupDirections []models.CombineDirection

	for _, grid := range multiStaticMap.Grid {
		var composite image.Image

		for _, m := range grid.Maps {
			mapPath := m.Map.Path()
			img, err := loadImage(mapPath)
			if err != nil {
				return fmt.Errorf("failed to load map %s: %w", mapPath, err)
			}

			if m.Direction == models.CombineDirectionFirst || composite == nil {
				// First image in the group — this is the anchor.
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
		return fmt.Errorf("no images to combine")
	}

	// Combine all grid groups together.
	result := groupImages[0]
	for i := 1; i < len(groupImages); i++ {
		dir := groupDirections[i]
		if dir == models.CombineDirectionFirst {
			dir = models.CombineDirectionRight
		}
		result = appendImages(result, groupImages[i], dir)
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

// loadImageWithRetry loads an image, retrying once via redownload if
// corrupted. Consults GlobalTileImageCache first — pprof showed PNG
// decode of already-disk-cached tiles dominating base-map stitching
// at 54% of CPU, and an in-memory LRU eliminates the repeat decode
// since the same tiles are requested heavily across adjacent
// staticmaps. A corrupted-file retry invalidates the cached entry.
func loadImageWithRetry(path string, redownload TileRedownloader) (image.Image, error) {
	if services.GlobalTileImageCache != nil {
		if img, ok := services.GlobalTileImageCache.Get(path); ok {
			return img, nil
		}
	}

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

	if err == nil && img != nil && services.GlobalTileImageCache != nil {
		services.GlobalTileImageCache.Add(path, img)
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
		encoder := &png.Encoder{CompressionLevel: png.BestCompression}
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

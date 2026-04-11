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
	combined := image.NewRGBA(image.Rect(0, 0, gridWidth, gridHeight))

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
	cropped := image.NewRGBA(image.Rect(0, 0, imgWidth, imgHeight))
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

// GenerateMultiStaticMapNative combines multiple static maps into a grid using native Go
func GenerateMultiStaticMapNative(multiStaticMap models.MultiStaticMap, path string) error {
	// First pass: load all images and calculate total dimensions
	type gridImage struct {
		img       image.Image
		direction models.CombineDirection
	}

	var rows [][]gridImage
	totalHeight := 0
	maxRowWidth := 0

	for _, grid := range multiStaticMap.Grid {
		var rowImages []gridImage
		rowWidth := 0
		rowHeight := 0

		for _, m := range grid.Maps {
			mapPath := m.Map.Path()
			img, err := loadImage(mapPath)
			if err != nil {
				return fmt.Errorf("failed to load map %s: %w", mapPath, err)
			}

			rowImages = append(rowImages, gridImage{img, m.Direction})

			if m.Direction == models.CombineDirectionBottom {
				if img.Bounds().Dx() > rowWidth {
					rowWidth = img.Bounds().Dx()
				}
				rowHeight += img.Bounds().Dy()
			} else {
				rowWidth += img.Bounds().Dx()
				if img.Bounds().Dy() > rowHeight {
					rowHeight = img.Bounds().Dy()
				}
			}
		}

		rows = append(rows, rowImages)
		if grid.Direction == models.CombineDirectionBottom {
			totalHeight += rowHeight
			if rowWidth > maxRowWidth {
				maxRowWidth = rowWidth
			}
		} else {
			if rowHeight > totalHeight {
				totalHeight = rowHeight
			}
			maxRowWidth += rowWidth
		}
	}

	// Create final image
	result := image.NewRGBA(image.Rect(0, 0, maxRowWidth, totalHeight))

	// Second pass: draw images
	currentY := 0
	currentX := 0

	for gridIdx, grid := range multiStaticMap.Grid {
		rowImages := rows[gridIdx]
		rowX := currentX
		rowY := currentY
		rowHeight := 0

		for _, gi := range rowImages {
			bounds := gi.img.Bounds()

			draw.Draw(result, image.Rect(rowX, rowY, rowX+bounds.Dx(), rowY+bounds.Dy()),
				gi.img, image.Point{}, draw.Src)

			if gi.direction == models.CombineDirectionBottom {
				rowY += bounds.Dy()
				if bounds.Dy() > rowHeight {
					rowHeight = bounds.Dy()
				}
			} else {
				rowX += bounds.Dx()
				if bounds.Dy() > rowHeight {
					rowHeight = bounds.Dy()
				}
			}
		}

		if grid.Direction == models.CombineDirectionBottom {
			currentY += rowHeight
		} else {
			currentX = rowX
		}
	}

	return saveImage(path, result)
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

// loadImageWithRetry loads an image, retrying once via redownload if corrupted
func loadImageWithRetry(path string, redownload TileRedownloader) (image.Image, error) {
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
	return img, err
}

// saveImage saves an image to a file
func saveImage(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg":
		quality := 90
		if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
			quality = services.GlobalImageSettings.ImageQuality
		}
		return jpeg.Encode(f, img, &jpeg.Options{Quality: quality})
	case ".webp":
		quality := 90
		if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
			quality = services.GlobalImageSettings.ImageQuality
		}
		return webp.Encode(f, img, webp.Options{Quality: quality})
	default:
		// Use configurable PNG compression level
		encoder := &png.Encoder{CompressionLevel: png.BestCompression}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		return encoder.Encode(f, img)
	}
}

// resizeImage resizes an image to the target dimensions
func resizeImage(img image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
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

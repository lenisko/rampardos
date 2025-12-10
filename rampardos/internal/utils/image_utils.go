package utils

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/lenisko/rampardos/internal/models"
)

const (
	imagemagickConvertCommand = "/usr/bin/convert"
)

// UseLegacyGraphicsEngine controls whether to use ImageMagick (true) or native Go (false)
var UseLegacyGraphicsEngine = false

// GenerateBaseStaticMap combines tiles into a base static map
func GenerateBaseStaticMap(staticMap models.StaticMap, tilePaths []string, path string, offsetX, offsetY int, hasScale bool) error {
	if !UseLegacyGraphicsEngine {
		return GenerateBaseStaticMapNative(staticMap, tilePaths, path, offsetX, offsetY, hasScale)
	}
	return generateBaseStaticMapImageMagick(staticMap, tilePaths, path, offsetX, offsetY, hasScale)
}

// generateBaseStaticMapImageMagick combines tiles using ImageMagick
func generateBaseStaticMapImageMagick(staticMap models.StaticMap, tilePaths []string, path string, offsetX, offsetY int, hasScale bool) error {
	var args []string

	if len(tilePaths) == 1 {
		args = tilePaths
	} else {
		// Sort tile paths for consistent ordering
		sortedPaths := make([]string, len(tilePaths))
		copy(sortedPaths, tilePaths)
		sort.Strings(sortedPaths)

		var lastY *int
		currentSegment := 0
		var segments [][]string

		for _, tilePath := range sortedPaths {
			if len(segments) == 0 {
				segments = append(segments, []string{"("})
			}

			split := strings.Split(tilePath, "-")
			y, _ := strconv.Atoi(split[len(split)-3])

			if lastY != nil {
				if y == *lastY {
					segments[currentSegment] = append(segments[currentSegment], tilePath, "-append")
				} else {
					segments[currentSegment] = append(segments[currentSegment], ")")
					if len(segments) != 1 {
						segments[currentSegment] = append(segments[currentSegment], "+append")
					}
					currentSegment++
					segments = append(segments, []string{"(", tilePath})
				}
			} else {
				segments[currentSegment] = append(segments[currentSegment], tilePath)
			}
			lastY = &y
		}

		segments[currentSegment] = append(segments[currentSegment], ")", "+append")

		for _, seg := range segments {
			args = append(args, seg...)
		}
	}

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

	args = append(args,
		"-crop", fmt.Sprintf("%dx%d+%d+%d", imgWidth, imgHeight, imgWidthOffset, imgHeightOffset),
		"+repage", path,
	)

	cmd := exec.Command(imagemagickConvertCommand, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Failed to run magick", "error", err, "output", string(output))
		removeImages(append(tilePaths, path))
		return fmt.Errorf("ImageMagick Error: %s", string(output))
	}

	return nil
}

// GenerateStaticMap adds markers, polygons, and circles to a base map
func GenerateStaticMap(staticMap models.StaticMap, basePath, path string, sm *SphericalMercator) error {
	if !UseLegacyGraphicsEngine {
		return GenerateStaticMapNative(staticMap, basePath, path, sm)
	}
	return generateStaticMapImageMagick(staticMap, basePath, path, sm)
}

// generateStaticMapImageMagick adds markers, polygons, and circles using ImageMagick
func generateStaticMapImageMagick(staticMap models.StaticMap, basePath, path string, sm *SphericalMercator) error {
	var args []string
	args = append(args, basePath)

	// Process polygons
	for _, polygon := range staticMap.Polygons {
		var points []string
		for _, coord := range polygon.Path {
			if len(coord) != 2 {
				return fmt.Errorf("expecting two values to form a coordinate but got %d", len(coord))
			}
			point := getRealOffset(
				models.Coordinate{Latitude: coord[0], Longitude: coord[1]},
				models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
				staticMap.Zoom, staticMap.Scale, 0, 0, sm,
			)
			x := point.x + int(staticMap.Width/2*uint16(staticMap.Scale))
			y := point.y + int(staticMap.Height/2*uint16(staticMap.Scale))
			points = append(points, fmt.Sprintf("%d,%d", x, y))
		}

		args = append(args,
			"-strokewidth", strconv.Itoa(int(polygon.StrokeWidth)),
			"-fill", polygon.FillColor,
			"-stroke", polygon.StrokeColor,
			"-gravity", "Center",
			"-draw", fmt.Sprintf("polygon %s", strings.Join(points, " ")),
		)
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
		radius := getRealOffset(coord, radiusCoord, staticMap.Zoom, staticMap.Scale, 0, 0, sm).y

		x := point.x + int(staticMap.Width)*int(staticMap.Scale)/2
		y := point.y + int(staticMap.Height)*int(staticMap.Scale)/2

		args = append(args,
			"-strokewidth", strconv.Itoa(int(circle.StrokeWidth)),
			"-fill", circle.FillColor,
			"-stroke", circle.StrokeColor,
			"-gravity", "Center",
			"-draw", fmt.Sprintf("circle %d,%d %d,%d", x, y, x, y+radius),
		)
	}

	// Process markers
	var markerPaths []string
	for _, marker := range staticMap.Markers {
		realOffset := getRealOffset(
			models.Coordinate{Latitude: marker.Latitude, Longitude: marker.Longitude},
			models.Coordinate{Latitude: staticMap.Latitude, Longitude: staticMap.Longitude},
			staticMap.Zoom, staticMap.Scale, int(marker.XOffset), int(marker.YOffset), sm,
		)

		// Skip markers outside the visible area
		if abs(realOffset.x) > int(staticMap.Width+marker.Width)*int(staticMap.Scale)/2 ||
			abs(realOffset.y) > int(staticMap.Height+marker.Height)*int(staticMap.Scale)/2 {
			continue
		}

		markerPath := getMarkerPath(marker)
		// Check for fallback
		if marker.FallbackURL != "" {
			if _, err := os.Stat(markerPath); os.IsNotExist(err) {
				markerPath = getFallbackMarkerPath(marker)
			}
		}

		markerPaths = append(markerPaths, markerPath)

		xPrefix := "+"
		if realOffset.x < 0 {
			xPrefix = ""
		}
		yPrefix := "+"
		if realOffset.y < 0 {
			yPrefix = ""
		}

		args = append(args,
			"(", markerPath, "-resize", fmt.Sprintf("%dx%d", marker.Width*uint16(staticMap.Scale), marker.Height*uint16(staticMap.Scale)), ")",
			"-gravity", "Center",
			"-geometry", fmt.Sprintf("%s%d%s%d", xPrefix, realOffset.x, yPrefix, realOffset.y),
			"-composite",
		)
	}

	args = append(args, path)

	cmd := exec.Command(imagemagickConvertCommand, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Failed to run magick", "error", err, "output", string(output))
		removeImages(append([]string{basePath, path}, markerPaths...))
		return fmt.Errorf("ImageMagick Error: %s", string(output))
	}

	return nil
}

// GenerateMultiStaticMap combines multiple static maps into a grid
func GenerateMultiStaticMap(multiStaticMap models.MultiStaticMap, path string) error {
	if !UseLegacyGraphicsEngine {
		return GenerateMultiStaticMapNative(multiStaticMap, path)
	}
	return generateMultiStaticMapImageMagick(multiStaticMap, path)
}

// generateMultiStaticMapImageMagick combines multiple static maps using ImageMagick
func generateMultiStaticMapImageMagick(multiStaticMap models.MultiStaticMap, path string) error {
	var args []string
	var mapPaths []string

	for _, grid := range multiStaticMap.Grid {
		var firstMapURL string
		var images []struct {
			direction models.CombineDirection
			path      string
		}

		for _, m := range grid.Maps {
			url := m.Map.Path()
			if m.Direction == models.CombineDirectionFirst {
				firstMapURL = url
			} else {
				images = append(images, struct {
					direction models.CombineDirection
					path      string
				}{m.Direction, url})
			}
			mapPaths = append(mapPaths, url)
		}

		args = append(args, "(", firstMapURL)
		for _, img := range images {
			args = append(args, img.path)
			if img.direction == models.CombineDirectionBottom {
				args = append(args, "-append")
			} else {
				args = append(args, "+append")
			}
		}
		args = append(args, ")")
		if grid.Direction == models.CombineDirectionBottom {
			args = append(args, "-append")
		} else {
			args = append(args, "+append")
		}
	}

	args = append(args, path)

	cmd := exec.Command(imagemagickConvertCommand, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Failed to run magick", "error", err, "output", string(output))
		removeImages(append([]string{path}, mapPaths...))
		return fmt.Errorf("ImageMagick Error: %s", string(output))
	}

	return nil
}

type offsetResult struct {
	x, y int
}

func getRealOffset(at, center models.Coordinate, zoom float64, scale uint8, extraX, extraY int, sm *SphericalMercator) offsetResult {
	var realOffsetX, realOffsetY int

	if center.Latitude == at.Latitude && center.Longitude == at.Longitude {
		realOffsetX = 0
		realOffsetY = 0
	} else {
		px1 := sm.Px(center, 20)
		px2 := sm.Px(at, 20)
		pxScale := math.Pow(2, zoom-20)
		realOffsetX = int((px2.X - px1.X) * pxScale * float64(scale))
		realOffsetY = int((px2.Y - px1.Y) * pxScale * float64(scale))
	}

	return offsetResult{
		x: realOffsetX + extraX*int(scale),
		y: realOffsetY + extraY*int(scale),
	}
}

func getMarkerPath(marker models.Marker) string {
	if strings.HasPrefix(marker.URL, "http://") || strings.HasPrefix(marker.URL, "https://") {
		hash := PersistentHashString(marker.URL)
		parts := strings.Split(marker.URL, ".")
		format := "png"
		if len(parts) > 0 {
			format = parts[len(parts)-1]
		}
		return fmt.Sprintf("Cache/Marker/%s.%s", hash, format)
	}
	return fmt.Sprintf("Markers/%s", marker.URL)
}

func getFallbackMarkerPath(marker models.Marker) string {
	if strings.HasPrefix(marker.FallbackURL, "http://") || strings.HasPrefix(marker.FallbackURL, "https://") {
		hash := PersistentHashString(marker.FallbackURL)
		parts := strings.Split(marker.FallbackURL, ".")
		format := "png"
		if len(parts) > 0 {
			format = parts[len(parts)-1]
		}
		return fmt.Sprintf("Cache/Marker/%s.%s", hash, format)
	}
	return fmt.Sprintf("Markers/%s", marker.FallbackURL)
}

func removeImages(paths []string) {
	var cachePaths []string
	for _, p := range paths {
		if strings.HasPrefix(p, "Cache/") {
			cachePaths = append(cachePaths, p)
		}
	}
	if len(cachePaths) > 0 {
		slog.Info("Clearing potentially broken images", "count", len(cachePaths))
		for _, p := range cachePaths {
			os.Remove(p)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

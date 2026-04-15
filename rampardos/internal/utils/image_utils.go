package utils

import (
	"fmt"
	"math"
	"strings"

	"github.com/lenisko/rampardos/internal/models"
)

// TileRedownloader is a callback to re-download a corrupted tile
type TileRedownloader func(tilePath string) error

// GenerateBaseStaticMap combines tiles into a base static map
func GenerateBaseStaticMap(staticMap models.StaticMap, tilePaths []string, path string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) error {
	return GenerateBaseStaticMapNative(staticMap, tilePaths, path, offsetX, offsetY, hasScale, redownload)
}

// ComposeBaseStaticMapBytes stitches tile images and crops to the
// requested viewport, returning the encoded result. Equivalent to
// GenerateBaseStaticMap but with no disk write — callers decide
// whether to persist the returned bytes.
func ComposeBaseStaticMapBytes(staticMap models.StaticMap, tilePaths []string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) ([]byte, error) {
	return composeBaseStaticMapBytesNative(staticMap, tilePaths, offsetX, offsetY, hasScale, redownload)
}

// GenerateStaticMap adds markers, polygons, and circles to a base map
func GenerateStaticMap(staticMap models.StaticMap, basePath, path string, sm *SphericalMercator) error {
	return GenerateStaticMapNative(staticMap, basePath, path, sm)
}

// GenerateStaticMapBytes draws overlays (markers/polygons/circles) onto
// an already-encoded base image in memory and returns the encoded result.
// No disk access for the base — callers hold the base bytes, so external
// file deletion cannot affect this path. Markers may be supplied via
// markerBytes (keyed by getMarkerPath(marker)); absent keys fall back to
// the disk read in drawOverlays for legacy callers.
func GenerateStaticMapBytes(staticMap models.StaticMap, baseBytes []byte, markerBytes map[string][]byte, sm *SphericalMercator) ([]byte, error) {
	return generateStaticMapBytesNative(staticMap, baseBytes, markerBytes, sm)
}

// GenerateMultiStaticMap combines multiple static maps into a grid
func GenerateMultiStaticMap(multiStaticMap models.MultiStaticMap, path string) error {
	return GenerateMultiStaticMapNative(multiStaticMap, path)
}

// ComposeMultiStaticMapBytes composes pre-encoded component images
// into the final grid image. componentBytes must be in grid iteration
// order (outer: grids, inner: maps within each grid, flattened).
func ComposeMultiStaticMapBytes(multiStaticMap models.MultiStaticMap, componentBytes [][]byte) ([]byte, error) {
	return composeMultiStaticMapBytesNative(multiStaticMap, componentBytes)
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

// GetMarkerPath returns the cache path for the given marker's primary URL.
// Remote markers (http/https) are stored as Cache/Marker/<hash>.<ext>;
// bundled markers are stored as Markers/<filename>.
func GetMarkerPath(marker models.Marker) string {
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

// GetFallbackMarkerPath returns the cache path for the given marker's fallback URL.
func GetFallbackMarkerPath(marker models.Marker) string {
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

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

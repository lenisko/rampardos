package utils

import (
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/lenisko/rampardos/internal/models"
)

// TileRedownloader is a callback to re-download a corrupted tile
type TileRedownloader func(tilePath string) error

// GenerateBaseStaticMap combines tiles into a base static map image.
// Caller owns persistence; the returned image can be cached in the
// composite LRU and served from memory.
func GenerateBaseStaticMap(staticMap models.StaticMap, tilePaths []string, offsetX, offsetY int, hasScale bool, redownload TileRedownloader) (image.Image, error) {
	return GenerateBaseStaticMapNative(staticMap, tilePaths, offsetX, offsetY, hasScale, redownload)
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

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

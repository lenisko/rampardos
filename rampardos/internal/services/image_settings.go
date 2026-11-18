package services

import (
	png "github.com/lenisko/rampardos/internal/utils/pngfast"

	"github.com/lenisko/rampardos/internal/config"
	"github.com/lenisko/rampardos/internal/models"
)

// ImageSettings holds global image processing settings
type ImageSettings struct {
	DefaultFormat        string // "png", "jpeg", or "webp"
	OverrideClientFormat bool   // If true, ignore client format
	PNGCompressionLevel  png.CompressionLevel
	PNGFilter            png.Filter
	ImageQuality         int // 1-100 for JPEG/WebP
}

// GlobalImageSettings is the global image settings instance
var GlobalImageSettings *ImageSettings

// InitImageSettings initializes global image settings from config
func InitImageSettings(cfg *config.Config) {
	settings := &ImageSettings{
		DefaultFormat:        cfg.DefaultImageFormat,
		OverrideClientFormat: cfg.OverrideClientFormat,
		PNGCompressionLevel:  png.BestCompression, // default
		ImageQuality:         cfg.ImageQuality,
	}

	switch cfg.PNGCompressionLevel {
	case "fast":
		settings.PNGCompressionLevel = png.BestSpeed
	case "none":
		settings.PNGCompressionLevel = png.NoCompression
	case "default":
		settings.PNGCompressionLevel = png.DefaultCompression
		// "best" or anything else uses BestCompression
	}

	// Row filter: up by default. Choosing a filter per row (adaptive,
	// stdlib's way) cost more CPU than the compression itself on map
	// output; a fixed up filter is 2.5-3x faster and about the same
	// size. Paeth keeps every output within 1% of adaptive's size at
	// about 1.6x faster, for installs that serve mostly multi-maps or
	// tiles, where up is 3-8% larger.
	switch cfg.PNGFilter {
	case "paeth":
		settings.PNGFilter = png.FilterPaeth
	case "adaptive":
		settings.PNGFilter = png.FilterAdaptive
	default:
		settings.PNGFilter = png.FilterUp
	}

	GlobalImageSettings = settings

	// Set the global default image format in models package
	switch cfg.DefaultImageFormat {
	case "jpeg", "jpg":
		models.SetDefaultImageFormat(models.ImageFormatJPEG)
	case "webp":
		models.SetDefaultImageFormat(models.ImageFormatWEBP)
	default:
		models.SetDefaultImageFormat(models.ImageFormatPNG)
	}

	// Set override flag
	models.SetOverrideClientFormat(cfg.OverrideClientFormat)
}

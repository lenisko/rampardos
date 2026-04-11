package services

import (
	"image/png"

	"github.com/lenisko/rampardos/internal/config"
	"github.com/lenisko/rampardos/internal/models"
)

// ImageSettings holds global image processing settings
type ImageSettings struct {
	DefaultFormat        string // "png", "jpeg", or "webp"
	OverrideClientFormat bool   // If true, ignore client format
	PNGCompressionLevel  png.CompressionLevel
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

	GlobalImageSettings = settings

	// Set the global default image format in models package
	switch cfg.DefaultImageFormat {
	case "jpeg", "jpg":
		models.DefaultImageFormat = models.ImageFormatJPEG
	case "webp":
		models.DefaultImageFormat = models.ImageFormatWEBP
	default:
		models.DefaultImageFormat = models.ImageFormatPNG
	}

	// Set override flag
	models.OverrideClientFormat = cfg.OverrideClientFormat
}

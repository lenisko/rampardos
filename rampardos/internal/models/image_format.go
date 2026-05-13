package models

import "sync"

// ImageFormat represents supported image formats
type ImageFormat string

const (
	ImageFormatPNG  ImageFormat = "png"
	ImageFormatJPG  ImageFormat = "jpg"
	ImageFormatJPEG ImageFormat = "jpeg"
	ImageFormatWEBP ImageFormat = "webp"
)

var (
	defaultImageFormatMu sync.RWMutex
	defaultImageFormat   ImageFormat = ImageFormatPNG

	overrideClientFormatMu sync.RWMutex
	overrideClientFormat   bool
)

// GetDefaultImageFormat returns the current default image format, safe for concurrent use.
func GetDefaultImageFormat() ImageFormat {
	defaultImageFormatMu.RLock()
	defer defaultImageFormatMu.RUnlock()
	return defaultImageFormat
}

// SetDefaultImageFormat sets the default image format, safe for concurrent use.
func SetDefaultImageFormat(f ImageFormat) {
	defaultImageFormatMu.Lock()
	defer defaultImageFormatMu.Unlock()
	defaultImageFormat = f
}

// GetOverrideClientFormat returns whether client-specified formats are overridden.
func GetOverrideClientFormat() bool {
	overrideClientFormatMu.RLock()
	defer overrideClientFormatMu.RUnlock()
	return overrideClientFormat
}

// SetOverrideClientFormat sets the override-client-format flag.
func SetOverrideClientFormat(v bool) {
	overrideClientFormatMu.Lock()
	defer overrideClientFormatMu.Unlock()
	overrideClientFormat = v
}

// IsValid checks if the format is supported
func (f ImageFormat) IsValid() bool {
	switch f {
	case ImageFormatPNG, ImageFormatJPG, ImageFormatJPEG, ImageFormatWEBP:
		return true
	}
	return false
}

// ContentType returns the MIME type for the format
func (f ImageFormat) ContentType() string {
	switch f {
	case ImageFormatPNG:
		return "image/png"
	case ImageFormatJPG, ImageFormatJPEG:
		return "image/jpeg"
	case ImageFormatWEBP:
		return "image/webp"
	}
	return "application/octet-stream"
}

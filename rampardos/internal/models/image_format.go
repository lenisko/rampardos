package models

// ImageFormat represents supported image formats
type ImageFormat string

const (
	ImageFormatPNG  ImageFormat = "png"
	ImageFormatJPG  ImageFormat = "jpg"
	ImageFormatJPEG ImageFormat = "jpeg"
	ImageFormatWEBP ImageFormat = "webp"
)

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

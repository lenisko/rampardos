package utils

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
)

// imageQuality returns the configured JPEG/WebP quality (1-100) or 90
// if GlobalImageSettings is unset or zero. Matches saveImage.
func imageQuality() int {
	if services.GlobalImageSettings != nil && services.GlobalImageSettings.ImageQuality > 0 {
		return services.GlobalImageSettings.ImageQuality
	}
	return 90
}

// pngCompression returns the configured PNG compression level. Matches saveImage.
func pngCompression() png.CompressionLevel {
	if services.GlobalImageSettings != nil {
		return services.GlobalImageSettings.PNGCompressionLevel
	}
	return png.BestCompression
}

// decodeImage decodes PNG/JPEG/WEBP/GIF bytes via the stdlib image
// registry (format detection is handled by the imported decoders).
func decodeImage(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}

// encodeImage encodes img in the requested format, honouring
// services.GlobalImageSettings for JPEG/WebP quality and PNG compression
// so the bytes-first pipeline matches legacy saveImage behaviour.
func encodeImage(img image.Image, format models.ImageFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG, "":
		encoder := &png.Encoder{CompressionLevel: pngCompression()}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("png encode: %w", err)
		}
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: imageQuality()}); err != nil {
			return nil, fmt.Errorf("jpeg encode: %w", err)
		}
	case models.ImageFormatWEBP:
		if err := webp.Encode(&buf, img, webp.Options{Quality: imageQuality()}); err != nil {
			return nil, fmt.Errorf("webp encode: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported image format %q", format)
	}
	return buf.Bytes(), nil
}

// SaveImageBytes atomically writes encoded image bytes to path. Delegates
// to services.SaveBytesAtomic so the three historical atomic-write paths
// (here, services.DownloadBytes cache, handlers.atomicWriteFile) share
// one implementation.
func SaveImageBytes(path string, data []byte) error {
	return services.SaveBytesAtomic(path, data)
}

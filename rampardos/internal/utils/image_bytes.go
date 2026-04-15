package utils

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
)

// decodeImage decodes PNG/JPEG/WEBP/GIF bytes via the stdlib image
// registry (format detection is handled by the imported decoders).
func decodeImage(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}

// encodeImage encodes img in the requested format.
func encodeImage(img image.Image, format models.ImageFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG, "":
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("png encode: %w", err)
		}
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("jpeg encode: %w", err)
		}
	case models.ImageFormatWEBP:
		if err := webp.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("webp encode: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported image format %q", format)
	}
	return buf.Bytes(), nil
}

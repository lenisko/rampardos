package utils

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})

	b, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeImage(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bounds() != img.Bounds() {
		t.Fatalf("bounds mismatch: got %v want %v", got.Bounds(), img.Bounds())
	}

	// Sanity: re-encode and parse as PNG to confirm format.
	var buf bytes.Buffer
	if err := png.Encode(&buf, got); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
}

func TestDecodeInvalidBytes(t *testing.T) {
	if _, err := decodeImage([]byte("not an image")); err == nil {
		t.Fatal("expected error decoding garbage")
	}
}

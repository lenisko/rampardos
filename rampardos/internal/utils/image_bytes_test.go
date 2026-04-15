package utils

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
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

func TestGenerateStaticMapBytes_NoDrawables(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	baseBytes, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pngFmt := models.ImageFormatPNG
	sm := models.StaticMap{Width: 8, Height: 8, Zoom: 10, Latitude: 0, Longitude: 0, Format: &pngFmt}

	out, err := GenerateStaticMapBytes(sm, baseBytes, nil, NewSphericalMercator())
	if err != nil {
		t.Fatalf("GenerateStaticMapBytes: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty output")
	}
	if _, err := decodeImage(out); err != nil {
		t.Fatalf("output not decodable: %v", err)
	}
}

func TestComposeMultiStaticMapBytes_TwoHorizontal(t *testing.T) {
	red := uniformPNG(t, color.RGBA{R: 255, A: 255})
	blue := uniformPNG(t, color.RGBA{B: 255, A: 255})

	msm := models.MultiStaticMap{
		Grid: []models.DirectionedMultiStaticMap{
			{
				Direction: models.CombineDirectionFirst,
				Maps: []models.DirectionedStaticMap{
					{Map: models.StaticMap{}, Direction: models.CombineDirectionFirst},
					{Map: models.StaticMap{}, Direction: models.CombineDirectionRight},
				},
			},
		},
	}

	out, err := ComposeMultiStaticMapBytes(msm, [][]byte{red, blue})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	got, err := decodeImage(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got.Bounds().Dx() != 16 || got.Bounds().Dy() != 8 {
		t.Fatalf("bounds: got %v want 16x8", got.Bounds())
	}
}

// uniformPNG returns an 8x8 PNG filled with c.
func uniformPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	b, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func TestGenerateStaticMapBytes_FileWrapperStillWorks(t *testing.T) {
	// Legacy GenerateStaticMapNative file-path path must still work:
	// write a base to a temp dir, call the file-path wrapper, read the result.
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base.png")
	out := filepath.Join(tmp, "out.png")

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	b, err := encodeImage(img, models.ImageFormatPNG)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(base, b, 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}

	pngFmt2 := models.ImageFormatPNG
	sm := models.StaticMap{Width: 8, Height: 8, Zoom: 10, Latitude: 0, Longitude: 0, Format: &pngFmt2}
	if err := GenerateStaticMap(sm, base, out, NewSphericalMercator()); err != nil {
		t.Fatalf("GenerateStaticMap: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected output file: %v", err)
	}
}

package utils

import (
	"image"
	"image/color"
	"image/draw"
	"testing"
)

// TestYCbCrToNRGBAMatchesDrawDraw locks in that the hand-coded
// YCbCr→NRGBA converter produces byte-identical output to the
// generic image/draw.Draw path it replaces. Mapbox satellite tiles
// decode to *image.YCbCr, so this is the hot-path substitution.
func TestYCbCrToNRGBAMatchesDrawDraw(t *testing.T) {
	cases := []struct {
		name         string
		ratio        image.YCbCrSubsampleRatio
		w, h         int
		seedY, seedC byte
	}{
		{"444 small", image.YCbCrSubsampleRatio444, 16, 16, 0x40, 0x80},
		{"420 small", image.YCbCrSubsampleRatio420, 16, 16, 0xA0, 0x50},
		{"422 tile", image.YCbCrSubsampleRatio422, 256, 256, 0x30, 0xC0},
		{"440 odd size", image.YCbCrSubsampleRatio440, 17, 13, 0x77, 0x66},
		{"420 odd size", image.YCbCrSubsampleRatio420, 19, 11, 0x12, 0xF0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := synthesiseYCbCr(tc.w, tc.h, tc.ratio, tc.seedY, tc.seedC)

			want := image.NewNRGBA(src.Bounds())
			draw.Draw(want, want.Bounds(), src, src.Bounds().Min, draw.Src)

			got := ycbcrToNRGBA(src)

			if got.Bounds() != want.Bounds() {
				t.Fatalf("bounds mismatch: got %v want %v", got.Bounds(), want.Bounds())
			}

			if len(got.Pix) != len(want.Pix) {
				t.Fatalf("pix length mismatch: got %d want %d", len(got.Pix), len(want.Pix))
			}

			for i := range want.Pix {
				if got.Pix[i] != want.Pix[i] {
					y := i / got.Stride
					x := (i % got.Stride) / 4
					chan4 := i % 4
					t.Fatalf("pixel mismatch at (%d,%d) chan %d: got %d want %d",
						x, y, chan4, got.Pix[i], want.Pix[i])
				}
			}
		})
	}
}

// TestToNRGBADispatchesYCbCrFastPath verifies toNRGBA routes YCbCr
// through the fast converter rather than the generic draw.Draw
// fallback. We test via equality — the fast path must produce the
// same output as the slow path.
func TestToNRGBADispatchesYCbCrFastPath(t *testing.T) {
	src := synthesiseYCbCr(32, 32, image.YCbCrSubsampleRatio420, 0x55, 0xAA)

	want := image.NewNRGBA(src.Bounds())
	draw.Draw(want, want.Bounds(), src, src.Bounds().Min, draw.Src)

	got := toNRGBA(src)

	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds mismatch: got %v want %v", got.Bounds(), want.Bounds())
	}
	for i := range want.Pix {
		if got.Pix[i] != want.Pix[i] {
			t.Fatalf("pixel %d: got %d want %d", i, got.Pix[i], want.Pix[i])
		}
	}
}

// TestToNRGBAIdentityForNRGBA verifies an *image.NRGBA passes
// through toNRGBA without allocation.
func TestToNRGBAIdentityForNRGBA(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	src.Set(4, 4, color.NRGBA{R: 0xAB, G: 0xCD, B: 0xEF, A: 0x80})

	got := toNRGBA(src)
	if got != src {
		t.Fatalf("expected identity return, got different pointer")
	}
}

// BenchmarkYCbCrToNRGBA compares the hand-coded converter against
// the draw.Draw fallback on a tile-sized (256×256) YCbCr input —
// the shape mapbox satellite tiles decode to.
func BenchmarkYCbCrToNRGBA(b *testing.B) {
	src := synthesiseYCbCr(256, 256, image.YCbCrSubsampleRatio420, 0x55, 0xAA)

	b.Run("hand-coded", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = ycbcrToNRGBA(src)
		}
	})
	b.Run("draw.Draw", func(b *testing.B) {
		bounds := src.Bounds()
		for i := 0; i < b.N; i++ {
			dst := image.NewNRGBA(bounds)
			draw.Draw(dst, bounds, src, bounds.Min, draw.Src)
		}
	})
}

// synthesiseYCbCr builds a deterministic *image.YCbCr with varied
// plane content so subsampling ratios actually exercise the COffset
// math in the converter.
func synthesiseYCbCr(w, h int, ratio image.YCbCrSubsampleRatio, seedY, seedC byte) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), ratio)
	for i := range img.Y {
		img.Y[i] = seedY + byte(i)
	}
	for i := range img.Cb {
		img.Cb[i] = seedC + byte(i*3)
	}
	for i := range img.Cr {
		img.Cr[i] = seedC + byte(i*5)
	}
	return img
}

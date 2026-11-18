package pngfast

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zlib"
)

// mapLike is an opaque image with flat areas, edges and a noisy patch,
// so every filter sees both the content it suits and the content it
// does not.
func mapLike(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for y := range h {
		for x := range w {
			c := color.NRGBA{0xf2, 0xef, 0xe9, 0xff}
			switch {
			case x > w*2/3:
				c = color.NRGBA{uint8(r.IntN(256)), uint8(r.IntN(256)), uint8(r.IntN(256)), 0xff}
			case (x/17+y/23)%5 == 0:
				c = color.NRGBA{0xff, 0xff, 0xff, 0xff}
			case y%40 < 3:
				c = color.NRGBA{0xaa, 0xd3, 0xdf, 0xff}
			}
			m.SetNRGBA(x, y, c)
		}
	}
	return m
}

// rowFilters returns the filter-type byte of every row of an 8-bit
// truecolor PNG.
func rowFilters(t *testing.T, data []byte, width, bpp int) []byte {
	t.Helper()
	var idat []byte
	for p := 8; p < len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		if string(data[p+4:p+8]) == "IDAT" {
			idat = append(idat, data[p+8:p+8+n]...)
		}
		p += 12 + n
	}
	zr, err := zlib.NewReader(bytes.NewReader(idat))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	stride := 1 + width*bpp
	var out []byte
	for p := 0; p < len(raw); p += stride {
		out = append(out, raw[p])
	}
	return out
}

func TestFilterRoundTripsAndIsAppliedToEveryRow(t *testing.T) {
	src := mapLike(301, 97)
	for _, tc := range []struct {
		name   string
		filter Filter
		want   int // filter byte every row must carry; -1 for any
	}{
		{"adaptive", FilterAdaptive, -1},
		{"up", FilterUp, ftUp},
		{"paeth", FilterPaeth, ftPaeth},
	} {
		for _, level := range []CompressionLevel{BestSpeed, DefaultCompression, BestCompression} {
			var buf bytes.Buffer
			enc := &Encoder{CompressionLevel: level, Filter: tc.filter}
			if err := enc.Encode(&buf, src); err != nil {
				t.Fatalf("%s/%d: %v", tc.name, level, err)
			}

			got, err := png.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("%s/%d: decode: %v", tc.name, level, err)
			}
			for y := range src.Bounds().Dy() {
				for x := range src.Bounds().Dx() {
					if a, b := color.NRGBAModel.Convert(got.At(x, y)), src.At(x, y); a != b {
						t.Fatalf("%s/%d: pixel (%d,%d) = %v, want %v", tc.name, level, x, y, a, b)
					}
				}
			}

			if tc.want < 0 {
				continue
			}
			// Opaque NRGBA encodes as 8-bit RGB, 3 bytes a pixel.
			for y, f := range rowFilters(t, buf.Bytes(), src.Bounds().Dx(), 3) {
				if int(f) != tc.want {
					t.Fatalf("%s/%d: row %d has filter %d, want %d", tc.name, level, y, f, tc.want)
				}
			}
		}
	}
}

// A fixed filter must not apply to paletted images, which stdlib
// leaves unfiltered: filters rarely help them.
func TestFixedFilterLeavesPalettedImagesUnfiltered(t *testing.T) {
	m := image.NewPaletted(image.Rect(0, 0, 40, 10), color.Palette{color.Black, color.White})
	for x := range 40 {
		m.SetColorIndex(x, x%10, uint8(x%2))
	}
	var buf bytes.Buffer
	if err := (&Encoder{Filter: FilterUp}).Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for x := range 40 {
		if got.(*image.Paletted).ColorIndexAt(x, x%10) != uint8(x%2) {
			t.Fatalf("paletted pixel %d changed", x)
		}
	}
}

// BenchmarkEncodeFilter compares the filters on the README example
// maps. For a fuller comparison, point PNGFAST_BENCH_DIR at a folder of
// real outputs (static maps, tiles, poracle multi-maps).
func BenchmarkEncodeFilter(b *testing.B) {
	dir := os.Getenv("PNGFAST_BENCH_DIR")
	if dir == "" {
		dir = "../../../../.exampleimages"
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.png"))
	if len(files) == 0 {
		b.Skip("no images in " + dir)
	}
	var imgs []image.Image
	for _, f := range files {
		r, err := os.Open(f)
		if err != nil {
			b.Fatal(err)
		}
		m, err := png.Decode(r)
		r.Close()
		if err != nil {
			b.Fatal(err)
		}
		n := image.NewNRGBA(m.Bounds())
		draw.Draw(n, n.Bounds(), m, image.Point{}, draw.Src)
		imgs = append(imgs, n)
	}
	for _, f := range []struct {
		name   string
		filter Filter
	}{{"adaptive", FilterAdaptive}, {"up", FilterUp}, {"paeth", FilterPaeth}} {
		b.Run(f.name, func(b *testing.B) {
			enc := &Encoder{CompressionLevel: BestSpeed, Filter: f.filter}
			var size int
			for b.Loop() {
				size = 0
				for _, m := range imgs {
					var buf bytes.Buffer
					if err := enc.Encode(&buf, m); err != nil {
						b.Fatal(err)
					}
					size += buf.Len()
				}
			}
			b.ReportMetric(float64(size)/float64(len(imgs))/1024, "KB/img")
		})
	}
}

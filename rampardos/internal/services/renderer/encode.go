package renderer

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"strconv"
	"strings"
	"sync"

	"github.com/gen2brain/webp"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	png "github.com/lenisko/rampardos/internal/utils/pngfast"
)

// Encoding and pool-keying helpers shared by every renderer entry point.
// These live outside gopool.go because that file is behind the mln_ffi
// build tag; keeping them untagged means the package still compiles (and
// these stay testable) without the FFI toolchain.

// rendererPNGBufferPool reuses png.EncoderBuffer across encodes so
// the internal zlib writer and filter working buffers don't allocate
// fresh per call. Kept local to the renderer package — the utils
// package has its own pool for the HTTP-boundary encoder.
var rendererPNGBufferPool rendererPNGEncoderBufferPool

type rendererPNGEncoderBufferPool struct {
	pool sync.Pool
}

func (p *rendererPNGEncoderBufferPool) Get() *png.EncoderBuffer {
	if b, ok := p.pool.Get().(*png.EncoderBuffer); ok {
		return b
	}
	return nil
}

func (p *rendererPNGEncoderBufferPool) Put(buf *png.EncoderBuffer) {
	p.pool.Put(buf)
}

// poolKey builds the map key for a (style, scale) pool. Scale=1 pools
// use the bare styleID for backward compatibility with log messages
// and the canary render path.
func poolKey(styleID string, scale uint8) string {
	if scale <= 1 {
		return styleID
	}
	return styleID + "@" + strconv.FormatUint(uint64(scale), 10)
}

func parsePoolKey(key string) (styleID string, ratio int) {
	if i := strings.LastIndex(key, "@"); i >= 0 {
		if r, err := strconv.Atoi(key[i+1:]); err == nil {
			return key[:i], r
		}
	}
	return key, 1
}

// encodeRGBAImage is the shared encode path used by both the byte-
// based renderer.Render entry points and any caller that already has
// an *image.NRGBA in hand.
func encodeRGBAImage(img *image.NRGBA, format models.ImageFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case models.ImageFormatPNG:
		// Respect PNG_COMPRESSION_LEVEL. Default png.Encoder uses
		// flate level 6 — ~4-6× slower than the "fast" setting
		// applied everywhere else via saveImage. The renderer runs
		// this on every maplibre-native tile/viewport, so the
		// difference compounds. BufferPool reuses the internal
		// zlib+filter state across encodes.
		encoder := png.Encoder{CompressionLevel: png.BestSpeed, BufferPool: &rendererPNGBufferPool}
		if services.GlobalImageSettings != nil {
			encoder.CompressionLevel = services.GlobalImageSettings.PNGCompressionLevel
		}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("renderer: png encode: %w", err)
		}
	case models.ImageFormatJPG, models.ImageFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("renderer: jpeg encode: %w", err)
		}
	case models.ImageFormatWEBP:
		if err := webp.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("renderer: webp encode: %w", err)
		}
	default:
		return nil, fmt.Errorf("renderer: unsupported format %q", format)
	}
	return buf.Bytes(), nil
}

package renderer

import (
	"bytes"
	"testing"
)

func TestUnpremultiplyRGBAInPlace(t *testing.T) {
	pix := []byte{
		0, 0, 0, 0, // transparent
		10, 20, 30, 255, // opaque: unchanged
		64, 32, 0, 128, // half alpha: channels double (rounded)
		1, 1, 1, 3, // low alpha: rounding
	}
	want := []byte{
		0, 0, 0, 0,
		10, 20, 30, 255,
		128, 64, 0, 128,
		85, 85, 85, 3,
	}

	unpremultiplyRGBA(pix)

	if !bytes.Equal(pix, want) {
		t.Errorf("got %v, want %v", pix, want)
	}
}

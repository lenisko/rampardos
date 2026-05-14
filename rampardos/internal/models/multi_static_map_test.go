package models

import (
	"strings"
	"testing"
)

// TestMultiStaticMap_PathHonoursFormat guards the bug that produced
// PNG-only multistaticmap responses regardless of DEFAULT_IMAGE_FORMAT
// / OVERRIDE_CLIENT_FORMAT: Path() previously hardcoded ".png" and
// bypassed the format-selection mechanism. The extension returned by
// Path() must match GetFormat() across the relevant precedence cases.
func TestMultiStaticMap_PathHonoursFormat(t *testing.T) {
	// Save and restore package-level config so the tests don't leak.
	prevDefault := GetDefaultImageFormat()
	prevOverride := GetOverrideClientFormat()
	t.Cleanup(func() {
		SetDefaultImageFormat(prevDefault)
		SetOverrideClientFormat(prevOverride)
	})

	pngPtr := func() *ImageFormat { f := ImageFormatPNG; return &f }
	webpPtr := func() *ImageFormat { f := ImageFormatWEBP; return &f }

	cases := []struct {
		name           string
		clientFormat   *ImageFormat
		serverDefault  ImageFormat
		serverOverride bool
		wantFormat     ImageFormat
	}{
		{"client unset, server default png", nil, ImageFormatPNG, false, ImageFormatPNG},
		{"client unset, server default webp", nil, ImageFormatWEBP, false, ImageFormatWEBP},
		{"client png, server default webp, no override", pngPtr(), ImageFormatWEBP, false, ImageFormatPNG},
		{"client png, server default webp, override on", pngPtr(), ImageFormatWEBP, true, ImageFormatWEBP},
		{"client webp, server default png, no override", webpPtr(), ImageFormatPNG, false, ImageFormatWEBP},
		{"client webp, server default png, override on", webpPtr(), ImageFormatPNG, true, ImageFormatPNG},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SetDefaultImageFormat(tc.serverDefault)
			SetOverrideClientFormat(tc.serverOverride)

			m := &MultiStaticMap{Format: tc.clientFormat}
			if got := m.GetFormat(); got != tc.wantFormat {
				t.Errorf("GetFormat() = %q, want %q", got, tc.wantFormat)
			}
			path := m.Path()
			wantSuffix := "." + string(tc.wantFormat)
			if !strings.HasSuffix(path, wantSuffix) {
				t.Errorf("Path() = %q, want suffix %q", path, wantSuffix)
			}
			// Sanity: path is under the StaticMulti cache dir
			if !strings.HasPrefix(path, "Cache/StaticMulti/") {
				t.Errorf("Path() = %q, expected Cache/StaticMulti/ prefix", path)
			}
		})
	}
}

// TestMultiStaticMap_HashIncludesFormat ensures distinct formats
// produce distinct cache entries, so a PNG response and a WebP
// response from the same grid don't collide on disk.
func TestMultiStaticMap_HashIncludesFormat(t *testing.T) {
	pngPtr := func() *ImageFormat { f := ImageFormatPNG; return &f }
	webpPtr := func() *ImageFormat { f := ImageFormatWEBP; return &f }

	a := &MultiStaticMap{Format: pngPtr()}
	b := &MultiStaticMap{Format: webpPtr()}

	if a.PersistentHash() == b.PersistentHash() {
		t.Errorf("identical grids with different formats hash to the same key: %s", a.PersistentHash())
	}
}

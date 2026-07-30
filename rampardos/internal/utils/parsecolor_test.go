package utils

import (
	"image/color"
	"testing"
)

// parseColor previously left unparsed components at zero, so malformed
// input rendered as a plausible-but-wrong colour instead of being
// rejected. These lock in that valid input is unaffected and invalid
// input is now visibly transparent rather than silently shifted.
func TestParseColorRejectsMalformed(t *testing.T) {
	valid := map[string]color.Color{
		"#ff0000":        color.NRGBA{255, 0, 0, 255},
		"rgb(0,128,255)": color.NRGBA{0, 128, 255, 255},
		"black":          color.Black,
		"transparent":    color.Transparent,
	}
	for in, want := range valid {
		if got := parseColor(in); got != want {
			t.Errorf("parseColor(%q) = %v, want %v", in, got, want)
		}
	}

	for _, in := range []string{"#ff00zz", "rgb(255,x,0)", "rgba(1,2,zz,1)"} {
		if got := parseColor(in); got != color.Transparent {
			t.Errorf("parseColor(%q) = %v, want Transparent (malformed input must not render a wrong colour)", in, got)
		}
	}
}

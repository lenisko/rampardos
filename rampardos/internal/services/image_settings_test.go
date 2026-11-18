package services

import (
	"testing"

	"github.com/lenisko/rampardos/internal/config"
	png "github.com/lenisko/rampardos/internal/utils/pngfast"
)

func TestPNGFilterSetting(t *testing.T) {
	prev := GlobalImageSettings
	t.Cleanup(func() { GlobalImageSettings = prev })

	for _, tc := range []struct {
		value string
		want  png.Filter
	}{
		{"", png.FilterUp},
		{"up", png.FilterUp},
		{"paeth", png.FilterPaeth},
		{"adaptive", png.FilterAdaptive},
		{"bogus", png.FilterUp},
	} {
		InitImageSettings(&config.Config{PNGFilter: tc.value})
		if got := GlobalImageSettings.PNGFilter; got != tc.want {
			t.Errorf("PNG_FILTER=%q: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

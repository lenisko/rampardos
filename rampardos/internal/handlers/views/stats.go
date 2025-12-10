package views

import (
	"net/http"
	"sort"

	"github.com/lenisko/rampardos/internal/services"
)

// StatsView renders the stats page
type StatsView struct {
	statsController *services.StatsController
	templates       *TemplateRenderer
}

// NewStatsView creates a new stats view
func NewStatsView(statsController *services.StatsController, templates *TemplateRenderer) *StatsView {
	return &StatsView{
		statsController: statsController,
		templates:       templates,
	}
}

// StatsContext is the template context for stats page
type StatsContext struct {
	BaseContext
	PageID             string
	PageName           string
	TileHitRatios      []RatioDisplay
	StaticMapHitRatios []RatioDisplay
	MarkerHitRatios    []RatioDisplay
}

// RatioDisplay represents a hit ratio for display
type RatioDisplay struct {
	Key   string
	Value string
}

// Render handles GET /admin/stats
func (v *StatsView) Render(w http.ResponseWriter, r *http.Request) {
	tileStats := v.statsController.GetTileStats()
	staticMapStats := v.statsController.GetStaticMapStats()
	markerStats := v.statsController.GetMarkerStats()

	var tileRatios []RatioDisplay
	for k, ratio := range tileStats {
		tileRatios = append(tileRatios, RatioDisplay{Key: k, Value: ratio.DisplayValue()})
	}
	sort.Slice(tileRatios, func(i, j int) bool { return tileRatios[i].Key < tileRatios[j].Key })

	var staticMapRatios []RatioDisplay
	for k, ratio := range staticMapStats {
		staticMapRatios = append(staticMapRatios, RatioDisplay{Key: k, Value: ratio.DisplayValue()})
	}
	sort.Slice(staticMapRatios, func(i, j int) bool { return staticMapRatios[i].Key < staticMapRatios[j].Key })

	var markerRatios []RatioDisplay
	for k, ratio := range markerStats {
		markerRatios = append(markerRatios, RatioDisplay{Key: k, Value: ratio.DisplayValue()})
	}
	sort.Slice(markerRatios, func(i, j int) bool { return markerRatios[i].Key < markerRatios[j].Key })

	ctx := StatsContext{
		BaseContext:        NewBaseContext(),
		PageID:             "stats",
		PageName:           "Stats",
		TileHitRatios:      tileRatios,
		StaticMapHitRatios: staticMapRatios,
		MarkerHitRatios:    markerRatios,
	}

	v.templates.Render(w, "stats.html", ctx)
}

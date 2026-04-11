package views

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/version"
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
	TileHitRatios      []CacheStatDisplay
	StaticMapHitRatios []CacheStatDisplay
	MarkerHitRatios    []CacheStatDisplay
	TemplateRenders    []TemplateRenderDisplay
	CacheSizes         []CacheSizeDisplay
	ErrorsByCategory   ErrorsByCategoryDisplay
	Memory             MemoryDisplay
	Uptime             string
	GitCommit          string
	TileserverRestarts uint64
}

// CacheSizeDisplay represents cache size for display
type CacheSizeDisplay struct {
	Folder      string
	Size        string
	DropPending bool
}

// CacheStatDisplay represents cache statistics for display
type CacheStatDisplay struct {
	Key     string
	Cached  uint64
	Total   uint64
	Percent string
}

// TemplateRenderDisplay represents template render stats for display
type TemplateRenderDisplay struct {
	Template string
	Method   string
	Type     string
	Count    uint64
}

// DailyErrorDisplay represents daily error count for display
type DailyErrorDisplay struct {
	Date    string
	Count   uint64
	Percent int
}

// ErrorCategoryDisplay represents a category of errors with its daily stats
type ErrorCategoryDisplay struct {
	Name   string
	Errors []DailyErrorDisplay
	Max    uint64
}

// ErrorsByCategoryDisplay holds all error categories for display
type ErrorsByCategoryDisplay struct {
	HTTP       ErrorCategoryDisplay
	Template   ErrorCategoryDisplay
	Validation ErrorCategoryDisplay
	Generic    ErrorCategoryDisplay
}

// MemoryDisplay represents memory statistics for display
type MemoryDisplay struct {
	RSS string
	VSS string
}

// Render handles GET /admin/stats
func (v *StatsView) Render(w http.ResponseWriter, r *http.Request) {
	tileStats := v.statsController.GetTileStats()
	staticMapStats := v.statsController.GetStaticMapStats()
	markerStats := v.statsController.GetMarkerStats()

	var tileRatios []CacheStatDisplay
	for k, ratio := range tileStats {
		tileRatios = append(tileRatios, CacheStatDisplay{
			Key:     k,
			Cached:  ratio.Cached,
			Total:   ratio.Total,
			Percent: ratio.PercentageString(),
		})
	}
	sort.Slice(tileRatios, func(i, j int) bool { return tileRatios[i].Key < tileRatios[j].Key })

	var staticMapRatios []CacheStatDisplay
	for k, ratio := range staticMapStats {
		staticMapRatios = append(staticMapRatios, CacheStatDisplay{
			Key:     k,
			Cached:  ratio.Cached,
			Total:   ratio.Total,
			Percent: ratio.PercentageString(),
		})
	}
	sort.Slice(staticMapRatios, func(i, j int) bool { return staticMapRatios[i].Key < staticMapRatios[j].Key })

	var markerRatios []CacheStatDisplay
	for k, ratio := range markerStats {
		markerRatios = append(markerRatios, CacheStatDisplay{
			Key:     k,
			Cached:  ratio.Cached,
			Total:   ratio.Total,
			Percent: ratio.PercentageString(),
		})
	}
	sort.Slice(markerRatios, func(i, j int) bool { return markerRatios[i].Key < markerRatios[j].Key })

	// Get template render stats
	var templateRenders []TemplateRenderDisplay
	for _, stat := range services.GlobalMetrics.GetTemplateRenderStats() {
		templateRenders = append(templateRenders, TemplateRenderDisplay{
			Template: stat.Template,
			Method:   stat.Method,
			Type:     stat.Type,
			Count:    stat.Count,
		})
	}
	sort.Slice(templateRenders, func(i, j int) bool {
		if templateRenders[i].Template != templateRenders[j].Template {
			return templateRenders[i].Template < templateRenders[j].Template
		}
		return templateRenders[i].Method < templateRenders[j].Method
	})

	// Get memory info
	memInfo := services.GlobalMetrics.GetMemoryInfo()
	memory := MemoryDisplay{
		RSS: formatBytes(memInfo.RSS),
		VSS: formatBytes(memInfo.VSS),
	}

	// Get uptime
	uptime := formatDuration(services.GlobalMetrics.GetUptime())

	// Get cache sizes
	pendingDrops := make(map[string]bool)
	for _, folder := range services.GetPendingDrops() {
		pendingDrops[folder] = true
	}

	var cacheSizes []CacheSizeDisplay
	for _, stat := range services.GlobalMetrics.GetCacheSizes() {
		cacheSizes = append(cacheSizes, CacheSizeDisplay{
			Folder:      stat.Folder,
			Size:        formatBytes(stat.Size),
			DropPending: pendingDrops[stat.Folder],
		})
	}
	sort.Slice(cacheSizes, func(i, j int) bool {
		return cacheSizes[i].Folder < cacheSizes[j].Folder
	})

	// Get daily errors by category
	errorData := services.GlobalMetrics.GetDailyErrorsByCategory()
	errorsByCategory := ErrorsByCategoryDisplay{
		HTTP:       buildErrorCategoryDisplay("HTTP", errorData.HTTP),
		Template:   buildErrorCategoryDisplay("Template", errorData.Template),
		Validation: buildErrorCategoryDisplay("Validation", errorData.Validation),
		Generic:    buildErrorCategoryDisplay("Generic", errorData.Generic),
	}

	ctx := StatsContext{
		BaseContext:        NewBaseContext(),
		PageID:             "stats",
		PageName:           "Stats",
		TileHitRatios:      tileRatios,
		StaticMapHitRatios: staticMapRatios,
		MarkerHitRatios:    markerRatios,
		TemplateRenders:    templateRenders,
		CacheSizes:         cacheSizes,
		ErrorsByCategory:   errorsByCategory,
		Memory:             memory,
		Uptime:             uptime,
		GitCommit:          version.ShortCommit(),
		TileserverRestarts: services.GlobalMetrics.GetTileserverRestarts(),
	}

	v.templates.Render(w, "stats.html", ctx)
}

// buildErrorCategoryDisplay converts service stats to display format with percentages
func buildErrorCategoryDisplay(name string, stats []services.DailyErrorStat) ErrorCategoryDisplay {
	var maxCount uint64
	for _, s := range stats {
		if s.Count > maxCount {
			maxCount = s.Count
		}
	}

	errors := make([]DailyErrorDisplay, len(stats))
	for i, s := range stats {
		percent := 0
		if maxCount > 0 {
			percent = int(s.Count * 100 / maxCount)
		}
		errors[i] = DailyErrorDisplay{
			Date:    s.Date,
			Count:   s.Count,
			Percent: percent,
		}
	}

	return ErrorCategoryDisplay{
		Name:   name,
		Errors: errors,
		Max:    maxCount,
	}
}

// formatBytes formats bytes to human-readable string
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatDuration formats duration to human-readable string
func formatDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

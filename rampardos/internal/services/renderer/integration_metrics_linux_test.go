//go:build renderer_integration

package renderer

import (
	"testing"

	"github.com/lenisko/rampardos/internal/services"
	"github.com/prometheus/client_golang/prometheus"
)

// histogramSamples returns the sample count and sum of the named
// histogram's series for (style, scale), or zeros if it has none yet.
func histogramSamples(t *testing.T, name, style, scale string) (uint64, float64) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["style"] == style && labels["scale"] == scale {
				return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
			}
		}
	}
	return 0, 0
}

// Pool sizing grows a pool only when a backlog would outlast a worker's
// startup, so every worker start must be timed, and a new worker's
// first render kept apart from warm ones to show its cold cost.
func TestWorkerStartupAndRenderMetrics(t *testing.T) {
	services.InitMetrics()
	cfg := setupIntegrationFixtures(t)

	const (
		startup = "rampardos_renderer_worker_startup_seconds"
		render  = "rampardos_renderer_worker_render_seconds"
		first   = "rampardos_renderer_worker_first_render_seconds"
	)
	startup0, startupSum0 := histogramSamples(t, startup, "bg", "1")
	render0, _ := histogramSamples(t, render, "bg", "1")
	first0, _ := histogramSamples(t, first, "bg", "1")

	r, err := NewGoPoolRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// The pool has one worker, so both renders hit it.
	runRenderAssertions(t, r)
	runRenderAssertions(t, r)

	startupN, startupSum := histogramSamples(t, startup, "bg", "1")
	renderN, _ := histogramSamples(t, render, "bg", "1")
	firstN, _ := histogramSamples(t, first, "bg", "1")
	if got := startupN - startup0; got != 1 {
		t.Errorf("worker startups recorded = %d, want 1", got)
	}
	if startupSum <= startupSum0 {
		t.Errorf("worker startup time sum did not rise: %v -> %v", startupSum0, startupSum)
	}
	if got := renderN - render0; got != 2 {
		t.Errorf("worker renders recorded = %d, want 2", got)
	}
	if got := firstN - first0; got != 1 {
		t.Errorf("worker first renders recorded = %d, want 1", got)
	}
}

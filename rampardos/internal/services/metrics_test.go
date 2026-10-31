package services

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// A served request goes through both the stats controller and
// RecordRequest; rampardos_requests_total must count it once.
func TestRequestsTotalCountedOncePerRequest(t *testing.T) {
	m := InitMetrics()
	s := NewStatsController(NewFileToucher())

	cases := []struct {
		reqType string
		serve   func()
	}{
		{"tile", func() { s.TileServed(true, "", "count-once") }},
		{"staticmap", func() { s.StaticMapServed(true, "", "count-once") }},
	}
	for _, c := range cases {
		requests := m.requestsTotal.WithLabelValues(c.reqType, bucketLabel("count-once"), "false")
		misses := m.cacheMissTotal.WithLabelValues(c.reqType)
		beforeReq, beforeMiss := testutil.ToFloat64(requests), testutil.ToFloat64(misses)

		c.serve()
		m.RecordRequest(c.reqType, "count-once", false, 0.01)

		if got := testutil.ToFloat64(requests) - beforeReq; got != 1 {
			t.Errorf("%s: requests_total rose by %v, want 1", c.reqType, got)
		}
		if got := testutil.ToFloat64(misses) - beforeMiss; got < 1 {
			t.Errorf("%s: cache_misses_total rose by %v, want at least 1", c.reqType, got)
		}
	}
}

// histogramBounds returns the bucket upper bounds of the first series of
// the named histogram in the default registry.
func histogramBounds(t *testing.T, name string) []float64 {
	t.Helper()
	h := histogramMetric(t, name, nil)
	var bounds []float64
	for _, b := range h.GetBucket() {
		bounds = append(bounds, b.GetUpperBound())
	}
	return bounds
}

// histogramMetric finds a series of the named histogram whose labels
// include every pair in labels (nil matches the first series).
func histogramMetric(t *testing.T, name string, labels map[string]string) *dto.Histogram {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					continue series
				}
			}
			return m.GetHistogram()
		}
	}
	t.Fatalf("histogram %s %v not found", name, labels)
	return nil
}

// Phase 2 pool sizing compares waits against a worker's startup time,
// tens to hundreds of milliseconds, so the waits must resolve there.
func TestRendererLatencyBuckets(t *testing.T) {
	m := InitMetrics()
	m.RecordRendererPoolAcquire("bkt", "1", 0.003)
	m.RecordRendererGlobalAcquire(0.003)
	m.DecRendererGlobalInFlight()
	m.RecordRendererWorkerStartup("bkt", "1", 0.2)
	m.RecordRendererWorkerRender("bkt", "1", 0.04, true)

	wait := []float64{0.00001, 0.001, 0.002, 0.005, 0.01, 0.02, 0.035, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5, 10}
	startup := []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 2, 5}
	render := []float64{0.005, 0.01, 0.02, 0.035, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5}
	for name, want := range map[string][]float64{
		"rampardos_renderer_pool_acquire_wait_seconds":   wait,
		"rampardos_renderer_global_acquire_wait_seconds": wait,
		"rampardos_renderer_worker_startup_seconds":      startup,
		"rampardos_renderer_worker_render_seconds":       render,
		"rampardos_renderer_worker_first_render_seconds": render,
	} {
		if got := histogramBounds(t, name); !slices.Equal(got, want) {
			t.Errorf("%s buckets = %v, want %v", name, got, want)
		}
	}
}

func TestRendererWorkerRenderFirstOnlyWhenFirst(t *testing.T) {
	m := InitMetrics()
	labels := map[string]string{"style": "firstonly", "scale": "1"}
	m.RecordRendererWorkerRender("firstonly", "1", 0.04, true)
	m.RecordRendererWorkerRender("firstonly", "1", 0.03, false)

	if got := histogramMetric(t, "rampardos_renderer_worker_render_seconds", labels).GetSampleCount(); got != 2 {
		t.Errorf("worker_render samples = %d, want 2", got)
	}
	if got := histogramMetric(t, "rampardos_renderer_worker_first_render_seconds", labels).GetSampleCount(); got != 1 {
		t.Errorf("worker_first_render samples = %d, want 1", got)
	}
}

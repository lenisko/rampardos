package services

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
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

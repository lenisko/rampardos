package models

import (
	"fmt"
	"sync/atomic"
)

// HitRatio tracks cache hit statistics
type HitRatio struct {
	Cached uint64 `json:"cached"`
	Total  uint64 `json:"total"`
}

// AtomicHitRatio is a thread-safe hit ratio counter
type AtomicHitRatio struct {
	cached atomic.Uint64
	total  atomic.Uint64
}

// Served records a served request (new=true means newly generated, not cached)
func (h *AtomicHitRatio) Served(new bool) {
	if !new {
		h.cached.Add(1)
	}
	h.total.Add(1)
}

// Get returns the current values
func (h *AtomicHitRatio) Get() HitRatio {
	return HitRatio{
		Cached: h.cached.Load(),
		Total:  h.total.Load(),
	}
}

// PercentageString returns the hit ratio as a percentage string
func (h HitRatio) PercentageString() string {
	if h.Total == 0 {
		return "0.00"
	}
	return fmt.Sprintf("%.2f", float64(h.Cached)/float64(h.Total)*100)
}

// DisplayValue returns a human-readable display value
func (h HitRatio) DisplayValue() string {
	return fmt.Sprintf("%d/%d (%s%%)", h.Cached, h.Total, h.PercentageString())
}

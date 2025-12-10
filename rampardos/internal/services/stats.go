package services

import (
	"sync"

	"github.com/lenisko/rampardos/internal/models"
)

// StatsController tracks cache hit statistics
type StatsController struct {
	fileToucher *FileToucher

	mu                 sync.RWMutex
	tileHitRatios      map[string]*models.AtomicHitRatio
	staticMapHitRatios map[string]*models.AtomicHitRatio
	markerHitRatios    map[string]*models.AtomicHitRatio
}

// NewStatsController creates a new stats controller
func NewStatsController(fileToucher *FileToucher) *StatsController {
	return &StatsController{
		fileToucher:        fileToucher,
		tileHitRatios:      make(map[string]*models.AtomicHitRatio),
		staticMapHitRatios: make(map[string]*models.AtomicHitRatio),
		markerHitRatios:    make(map[string]*models.AtomicHitRatio),
	}
}

// TileServed records a tile being served
func (s *StatsController) TileServed(isNew bool, path, style string) {
	if !isNew {
		s.fileToucher.Touch(path)
	}
	s.recordTile(isNew, style)
}

// StaticMapServed records a static map being served
func (s *StatsController) StaticMapServed(isNew bool, path, style string) {
	if !isNew {
		s.fileToucher.Touch(path)
	}
	s.recordStaticMap(isNew, style)
}

// MarkerServed records a marker being served
func (s *StatsController) MarkerServed(isNew bool, path, domain string) {
	if !isNew {
		s.fileToucher.Touch(path)
	}
	s.recordMarker(isNew, domain)
}

func (s *StatsController) recordTile(isNew bool, style string) {
	s.mu.Lock()
	if s.tileHitRatios[style] == nil {
		s.tileHitRatios[style] = &models.AtomicHitRatio{}
	}
	ratio := s.tileHitRatios[style]
	s.mu.Unlock()

	ratio.Served(isNew)
	GlobalMetrics.RecordTileRequest(style, !isNew)
}

func (s *StatsController) recordStaticMap(isNew bool, style string) {
	s.mu.Lock()
	if s.staticMapHitRatios[style] == nil {
		s.staticMapHitRatios[style] = &models.AtomicHitRatio{}
	}
	ratio := s.staticMapHitRatios[style]
	s.mu.Unlock()

	ratio.Served(isNew)
	GlobalMetrics.RecordStaticMapRequest(style, !isNew)
}

func (s *StatsController) recordMarker(isNew bool, domain string) {
	s.mu.Lock()
	if s.markerHitRatios[domain] == nil {
		s.markerHitRatios[domain] = &models.AtomicHitRatio{}
	}
	ratio := s.markerHitRatios[domain]
	s.mu.Unlock()

	ratio.Served(isNew)
	GlobalMetrics.RecordMarkerRequest(domain, !isNew)
}

// GetTileStats returns tile hit ratios
func (s *StatsController) GetTileStats() map[string]models.HitRatio {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]models.HitRatio)
	for k, v := range s.tileHitRatios {
		result[k] = v.Get()
	}
	return result
}

// GetStaticMapStats returns static map hit ratios
func (s *StatsController) GetStaticMapStats() map[string]models.HitRatio {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]models.HitRatio)
	for k, v := range s.staticMapHitRatios {
		result[k] = v.Get()
	}
	return result
}

// GetMarkerStats returns marker hit ratios
func (s *StatsController) GetMarkerStats() map[string]models.HitRatio {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]models.HitRatio)
	for k, v := range s.markerHitRatios {
		result[k] = v.Get()
	}
	return result
}

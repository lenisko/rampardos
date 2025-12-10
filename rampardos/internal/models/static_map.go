package models

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// StaticMap represents a static map request
type StaticMap struct {
	Style     string       `json:"style"`
	Latitude  float64      `json:"latitude"`
	Longitude float64      `json:"longitude"`
	Zoom      float64      `json:"zoom"`
	Width     uint16       `json:"width"`
	Height    uint16       `json:"height"`
	Scale     uint8        `json:"scale"`
	Format    *ImageFormat `json:"format,omitempty"`
	Bearing   *float64     `json:"bearing,omitempty"`
	Pitch     *float64     `json:"pitch,omitempty"`
	Markers   []Marker     `json:"markers,omitempty"`
	Polygons  []Polygon    `json:"polygons,omitempty"`
	Circles   []Circle     `json:"circles,omitempty"`
}

// GetFormat returns the format or default PNG
func (s *StaticMap) GetFormat() ImageFormat {
	if s.Format != nil {
		return *s.Format
	}
	return ImageFormatPNG
}

// Path returns the cache path for this static map
func (s *StaticMap) Path() string {
	return fmt.Sprintf("Cache/Static/%s.%s", s.PersistentHash(), s.GetFormat())
}

// PersistentHash generates a stable hash for cache key
func (s *StaticMap) PersistentHash() string {
	// Use sorted JSON for consistent hashing
	data, _ := json.Marshal(s)
	hash := sha256.Sum256(data)
	encoded := base64.StdEncoding.EncodeToString(hash[:])
	return strings.ReplaceAll(encoded, "/", "_")
}

// WithoutDrawables returns a copy without markers, polygons, circles
func (s *StaticMap) WithoutDrawables() StaticMap {
	return StaticMap{
		Style:     s.Style,
		Latitude:  s.Latitude,
		Longitude: s.Longitude,
		Zoom:      s.Zoom,
		Width:     s.Width,
		Height:    s.Height,
		Scale:     s.Scale,
		Format:    s.Format,
		Bearing:   s.Bearing,
		Pitch:     s.Pitch,
	}
}

// BasePath returns the cache path for the base map (without drawables)
func (s *StaticMap) BasePath() string {
	base := s.WithoutDrawables()
	return fmt.Sprintf("Cache/Static/%s.%s", base.PersistentHash(), s.GetFormat())
}

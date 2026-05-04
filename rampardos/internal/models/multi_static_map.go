package models

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// CombineDirection specifies how maps are combined
type CombineDirection string

const (
	CombineDirectionFirst  CombineDirection = "first"
	CombineDirectionRight  CombineDirection = "right"
	CombineDirectionBottom CombineDirection = "bottom"
)

// DirectionedStaticMap is a static map with a combine direction
type DirectionedStaticMap struct {
	Direction CombineDirection `json:"direction"`
	Map       StaticMap        `json:"map"`
}

// DirectionedMultiStaticMap is a grid row/column of maps
type DirectionedMultiStaticMap struct {
	Direction CombineDirection       `json:"direction"`
	Maps      []DirectionedStaticMap `json:"maps"`
}

// MultiStaticMap represents a grid of static maps
type MultiStaticMap struct {
	Grid   []DirectionedMultiStaticMap `json:"grid"`
	Format *ImageFormat                `json:"format,omitempty"`
}

// GetFormat returns the format the response should be encoded in.
// Mirrors StaticMap.GetFormat: client value if set, else server default;
// OverrideClientFormat trumps the client. Without this, MultiStaticMap
// previously hardcoded ".png" in Path() and bypassed the format-
// selection mechanism entirely — DEFAULT_IMAGE_FORMAT and
// OVERRIDE_CLIENT_FORMAT had no effect on the multistaticmap path,
// which is the dominant CPU consumer in Poracle workloads.
func (m *MultiStaticMap) GetFormat() ImageFormat {
	if m.Format != nil && !OverrideClientFormat {
		return *m.Format
	}
	return DefaultImageFormat
}

// Path returns the cache path for this multi static map. The extension
// derives from GetFormat(), so changing DEFAULT_IMAGE_FORMAT or
// OVERRIDE_CLIENT_FORMAT propagates to the cache key — different
// formats produce different bytes and therefore different cache
// entries.
func (m *MultiStaticMap) Path() string {
	return fmt.Sprintf("Cache/StaticMulti/%s.%s", m.PersistentHash(), m.GetFormat())
}

// PersistentHash generates a stable hash for cache key
func (m *MultiStaticMap) PersistentHash() string {
	data, _ := json.Marshal(m)
	hash := sha256.Sum256(data)
	encoded := base64.StdEncoding.EncodeToString(hash[:])
	return strings.ReplaceAll(encoded, "/", "_")
}

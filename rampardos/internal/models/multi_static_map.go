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
// GetOverrideClientFormat() trumps the client.
func (m *MultiStaticMap) GetFormat() ImageFormat {
	if m.Format != nil && !GetOverrideClientFormat() {
		return *m.Format
	}
	return GetDefaultImageFormat()
}

// Path returns the cache path for this multi static map. The extension
// derives from GetFormat(), so changing the default image format or
// override-client-format setting propagates to the cache key.
func (m *MultiStaticMap) Path() string {
	return fmt.Sprintf("Cache/StaticMulti/%s.%s", m.PersistentHash(), m.GetFormat())
}

// PersistentHash generates a stable hash for cache key.
// It normalizes each sub-map by delegating to StaticMap.PersistentHash()
// (which already handles Scale=0 vs 1 and Format=nil vs explicit) and
// assembles those hashes with the resolved format so that two
// functionally-identical MultiStaticMap values produce the same key.
func (m *MultiStaticMap) PersistentHash() string {
	type subEntry struct {
		Dir     CombineDirection `json:"dir"`
		MapHash string           `json:"map_hash"`
	}
	type rowEntry struct {
		Dir  CombineDirection `json:"dir"`
		Maps []subEntry       `json:"maps"`
	}
	type normalized struct {
		Grid   []rowEntry  `json:"grid"`
		Format ImageFormat `json:"format"`
	}

	norm := normalized{
		Grid:   make([]rowEntry, len(m.Grid)),
		Format: m.GetFormat(),
	}
	for i, row := range m.Grid {
		entries := make([]subEntry, len(row.Maps))
		for j, dsm := range row.Maps {
			entries[j] = subEntry{
				Dir:     dsm.Direction,
				MapHash: dsm.Map.PersistentHash(),
			}
		}
		norm.Grid[i] = rowEntry{Dir: row.Direction, Maps: entries}
	}

	data, _ := json.Marshal(norm)
	hash := sha256.Sum256(data)
	encoded := base64.StdEncoding.EncodeToString(hash[:])
	return strings.ReplaceAll(encoded, "/", "_")
}

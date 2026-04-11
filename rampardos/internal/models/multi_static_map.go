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
	Grid []DirectionedMultiStaticMap `json:"grid"`
}

// Path returns the cache path for this multi static map
func (m *MultiStaticMap) Path() string {
	return fmt.Sprintf("Cache/StaticMulti/%s.png", m.PersistentHash())
}

// PersistentHash generates a stable hash for cache key
func (m *MultiStaticMap) PersistentHash() string {
	data, _ := json.Marshal(m)
	hash := sha256.Sum256(data)
	encoded := base64.StdEncoding.EncodeToString(hash[:])
	return strings.ReplaceAll(encoded, "/", "_")
}

package models

// Polygon represents a polygon to be drawn on a static map
type Polygon struct {
	FillColor   string      `json:"fill_color"`
	StrokeColor string      `json:"stroke_color"`
	StrokeWidth uint8       `json:"stroke_width"`
	Path        [][]float64 `json:"path"` // Array of [lat, lon] pairs
}

package models

// Circle represents a circle to be drawn on a static map
type Circle struct {
	FillColor   string  `json:"fill_color"`
	StrokeColor string  `json:"stroke_color"`
	StrokeWidth uint8   `json:"stroke_width"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Radius      float64 `json:"radius"` // Radius in meters
}

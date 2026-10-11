package models

// Marker represents a marker to be drawn on a static map. Width and
// Height bound the marker box: the image is scaled to fit inside it
// with its aspect ratio kept, then centred on the point.
type Marker struct {
	URL         string  `json:"url"`
	FallbackURL string  `json:"fallback_url,omitempty"`
	Height      uint16  `json:"height"`
	Width       uint16  `json:"width"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	XOffset     int16   `json:"x_offset,omitempty"`
	YOffset     int16   `json:"y_offset,omitempty"`
}

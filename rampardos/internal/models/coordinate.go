package models

import "math"

// Coordinate represents a geographic coordinate
type Coordinate struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// CoordinateAt returns a new coordinate at a given distance and direction from this coordinate
// distance is in meters, direction is in radians (0 = north)
func (c Coordinate) CoordinateAt(distance float64, direction float64) Coordinate {
	dist := distance / 6_373_000.0
	lat := c.Latitude * math.Pi / 180.0
	lon := c.Longitude * math.Pi / 180.0

	otherLat := math.Asin(
		math.Sin(lat)*math.Cos(dist) + math.Cos(lat)*math.Sin(dist)*math.Cos(direction),
	)
	otherLon := lon + math.Atan2(
		math.Sin(direction)*math.Sin(dist)*math.Cos(lat),
		math.Cos(dist)-math.Sin(lat)*math.Sin(otherLat),
	)

	return Coordinate{
		Latitude:  otherLat * 180.0 / math.Pi,
		Longitude: otherLon * 180.0 / math.Pi,
	}
}

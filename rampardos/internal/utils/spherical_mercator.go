package utils

import (
	"math"

	"github.com/lenisko/rampardos/internal/models"
)

const (
	d2r       = math.Pi / 180
	r2d       = 180 / math.Pi
	a         = 6378137.0
	maxExtent = 20037508.342789244
	tileSize  = 256.0
)

// SphericalMercator handles coordinate projections
type SphericalMercator struct {
	bc []float64
	cc []float64
	zc []float64
	ac []float64
}

// Point represents an x,y pixel coordinate
type Point struct {
	X float64
	Y float64
}

// Bounds represents geographic bounds
type Bounds struct {
	WS models.Coordinate // West-South
	EN models.Coordinate // East-North
}

// XYZBounds represents tile bounds
type XYZBounds struct {
	MinPoint Point
	MaxPoint Point
}

// NewSphericalMercator creates a new SphericalMercator instance
func NewSphericalMercator() *SphericalMercator {
	sm := &SphericalMercator{
		bc: make([]float64, 30),
		cc: make([]float64, 30),
		zc: make([]float64, 30),
		ac: make([]float64, 30),
	}

	size := tileSize
	for i := range 30 {
		sm.bc[i] = size / 360
		sm.cc[i] = size / (2 * math.Pi)
		sm.zc[i] = size / 2
		sm.ac[i] = size
		size *= 2
	}

	return sm
}

// Px converts lon/lat to screen pixel value
func (sm *SphericalMercator) Px(coord models.Coordinate, zoom int) Point {
	d := sm.zc[zoom]
	f := math.Min(math.Max(math.Sin(d2r*coord.Latitude), -0.9999), 0.9999)
	x := math.Round(d + coord.Longitude*sm.bc[zoom])
	y := math.Round(d + 0.5*math.Log((1+f)/(1-f))*(-sm.cc[zoom]))

	if x > sm.ac[zoom] {
		x = sm.ac[zoom]
	}
	if y > sm.ac[zoom] {
		y = sm.ac[zoom]
	}

	return Point{X: x, Y: y}
}

// LL converts screen pixel value to Coordinate
func (sm *SphericalMercator) LL(px Point, zoom int) models.Coordinate {
	g := (px.Y - sm.zc[zoom]) / (-sm.cc[zoom])
	longitude := (px.X - sm.zc[zoom]) / sm.bc[zoom]
	latitude := r2d * (2*math.Atan(math.Exp(g)) - 0.5*math.Pi)
	return models.Coordinate{Latitude: latitude, Longitude: longitude}
}

// BBox converts tile xyz value to Bounds
func (sm *SphericalMercator) BBox(x, y float64, zoom int, tmsStyle bool, srs string) Bounds {
	_y := y
	if tmsStyle {
		_y = math.Pow(2, float64(zoom)) - 1 - y
	}

	ws := sm.LL(Point{X: x * tileSize, Y: (_y + 1) * tileSize}, zoom)
	en := sm.LL(Point{X: (x + 1) * tileSize, Y: _y * tileSize}, zoom)
	bounds := Bounds{WS: ws, EN: en}

	if srs == "900913" {
		return sm.Convert(bounds, "900913")
	}
	return bounds
}

// XYZ converts bounds to xyz bounds
func (sm *SphericalMercator) XYZ(bbox Bounds, zoom int, tmsStyle bool, srs string) XYZBounds {
	_bbox := bbox
	if srs == "900913" {
		_bbox = sm.Convert(bbox, "WGS84")
	}

	pxLL := sm.Px(_bbox.WS, zoom)
	pxUR := sm.Px(_bbox.EN, zoom)

	minX := math.Floor(pxLL.X / tileSize)
	maxX := math.Floor((pxUR.X - 1) / tileSize)
	minY := math.Floor(pxUR.Y / tileSize)
	maxY := math.Floor((pxLL.Y - 1) / tileSize)

	xyzBounds := XYZBounds{
		MinPoint: Point{X: math.Max(0, minX), Y: math.Max(0, minY)},
		MaxPoint: Point{X: maxX, Y: maxY},
	}

	if tmsStyle {
		zoomMax := math.Pow(2, float64(zoom)) - 1
		minY := zoomMax - xyzBounds.MaxPoint.Y
		maxY := zoomMax - xyzBounds.MinPoint.Y
		xyzBounds.MinPoint.Y = minY
		xyzBounds.MaxPoint.Y = maxY
	}

	return xyzBounds
}

// XY converts coordinate to tile xyz
func (sm *SphericalMercator) XY(coord models.Coordinate, zoom int) (x, y int, xDelta, yDelta int16) {
	pxC := sm.Px(coord, zoom)
	x = int(pxC.X / tileSize)
	xDelta = int16(math.Mod(pxC.X, tileSize))
	y = int(pxC.Y / tileSize)
	yDelta = int16(math.Mod(pxC.Y, tileSize))
	return
}

// Convert converts projection of given bbox
func (sm *SphericalMercator) Convert(bounds Bounds, srs string) Bounds {
	if srs == "900913" {
		point1 := sm.Forward(bounds.WS)
		point2 := sm.Forward(bounds.EN)
		return Bounds{
			WS: models.Coordinate{Latitude: point1.X, Longitude: point1.Y},
			EN: models.Coordinate{Latitude: point2.X, Longitude: point2.Y},
		}
	}
	point1 := sm.Inverse(Point{X: bounds.WS.Latitude, Y: bounds.WS.Longitude})
	point2 := sm.Inverse(Point{X: bounds.EN.Latitude, Y: bounds.EN.Longitude})
	return Bounds{WS: point1, EN: point2}
}

// Forward converts Coordinate to 900913 Point
func (sm *SphericalMercator) Forward(coord models.Coordinate) Point {
	x := a * coord.Longitude * d2r
	y := a * math.Log(math.Tan(math.Pi*0.25+0.5*coord.Latitude*d2r))

	x = math.Min(math.Max(x, -maxExtent), maxExtent)
	y = math.Min(math.Max(y, -maxExtent), maxExtent)

	return Point{X: x, Y: y}
}

// Inverse converts 900913 Point to Coordinate
func (sm *SphericalMercator) Inverse(point Point) models.Coordinate {
	return models.Coordinate{
		Latitude:  (math.Pi*0.5 - 2.0*math.Atan(math.Exp(-point.Y/a))) * r2d,
		Longitude: point.X * r2d / a,
	}
}

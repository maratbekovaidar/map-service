package geo

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// earthRadius is the mean Earth radius (IUGG), metres.
const earthRadius = 6371008.8

func rad(deg float64) float64 { return deg * math.Pi / 180 }
func deg(rad float64) float64 { return rad * 180 / math.Pi }

// Distance returns the great-circle (haversine) distance between a and b in
// metres.
func Distance(a, b Point) float64 {
	lat1, lat2 := rad(a.Lat), rad(b.Lat)
	dLat, dLon := lat2-lat1, rad(b.Lon-a.Lon)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadius * math.Asin(math.Sqrt(min(1, h)))
}

// Bearing returns the initial bearing from a to b in degrees clockwise from
// north, in [0, 360). It is 0 when the points coincide.
func Bearing(a, b Point) float64 {
	lat1, lat2 := rad(a.Lat), rad(b.Lat)
	dLon := rad(b.Lon - a.Lon)
	y := math.Sin(dLon) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLon)
	return NormBearing(deg(math.Atan2(y, x)))
}

// NormBearing maps any angle in degrees to [0, 360).
func NormBearing(b float64) float64 {
	b = math.Mod(b, 360)
	if b < 0 {
		b += 360
	}
	if b >= 360 { // -tiny + 360 rounds to 360
		b = 0
	}
	return b
}

// AngleDiff returns the absolute difference between two bearings in degrees,
// in [0, 180].
func AngleDiff(a, b float64) float64 {
	d := math.Abs(NormBearing(a) - NormBearing(b))
	return math.Min(d, 360-d)
}

// Destination returns the point dist metres from p along bearing (degrees).
func Destination(p Point, bearing, dist float64) Point {
	lat1, lon1, brg, ang := rad(p.Lat), rad(p.Lon), rad(bearing), dist/earthRadius
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(ang) + math.Cos(lat1)*math.Sin(ang)*math.Cos(brg))
	lon2 := lon1 + math.Atan2(math.Sin(brg)*math.Sin(ang)*math.Cos(lat1), math.Cos(ang)-math.Sin(lat1)*math.Sin(lat2))
	return Point{Lon: math.Mod(deg(lon2)+540, 360) - 180, Lat: deg(lat2)}
}

// Line is a polyline with precomputed distances along it, for following a
// position along a route. Segment i runs from point i to point i+1; repeated
// points (zero-length segments) are kept so that per-segment data such as
// OSRM annotations stays aligned.
type Line struct {
	pts []Point
	cum []float64 // cum[i]: distance along the line to pts[i], metres
}

// NewLine builds a Line from at least two valid points.
func NewLine(pts []Point) (*Line, error) {
	if len(pts) < 2 {
		return nil, errors.New("line: need at least two points")
	}
	cum := make([]float64, len(pts))
	for i, p := range pts {
		if !p.Valid() {
			return nil, fmt.Errorf("line: point %d is not a valid coordinate", i)
		}
		if i > 0 {
			cum[i] = cum[i-1] + Distance(pts[i-1], p)
		}
	}
	return &Line{pts: append([]Point(nil), pts...), cum: cum}, nil
}

// Points returns the vertices of the line. The slice must not be modified.
func (l *Line) Points() []Point { return l.pts }

// Length is the length of the line in metres.
func (l *Line) Length() float64 { return l.cum[len(l.cum)-1] }

// Segments is the number of segments (points - 1).
func (l *Line) Segments() int { return len(l.pts) - 1 }

// VertexAlong is the distance along the line to vertex i, metres.
func (l *Line) VertexAlong(i int) float64 { return l.cum[i] }

// Start and End are the first and the last point of the line.
func (l *Line) Start() Point { return l.pts[0] }

// End is the last point of the line.
func (l *Line) End() Point { return l.pts[len(l.pts)-1] }

// segmentAt returns the segment holding the point `along` metres from the
// start (clamped to the line); a vertex belongs to the segment it starts.
func (l *Line) segmentAt(along float64) int {
	i := sort.SearchFloat64s(l.cum, along) // first i with cum[i] >= along
	if i < len(l.cum) && l.cum[i] == along {
		return min(i, l.Segments()-1)
	}
	return max(0, min(i-1, l.Segments()-1))
}

// PointAt returns the point `along` metres from the start, clamped to the line.
func (l *Line) PointAt(along float64) Point {
	along = math.Max(0, math.Min(along, l.Length()))
	i := l.segmentAt(along)
	segLen := l.cum[i+1] - l.cum[i]
	if segLen == 0 {
		return l.pts[i]
	}
	return lerp(l.pts[i], l.pts[i+1], (along-l.cum[i])/segLen)
}

// BearingAt returns the direction of travel `along` metres from the start: the
// bearing of the segment there, or of the nearest non-empty segment when that
// one has zero length. It is 0 for a line of coincident points.
func (l *Line) BearingAt(along float64) float64 {
	i := l.segmentAt(math.Max(0, math.Min(along, l.Length())))
	for d := 0; d < len(l.pts); d++ {
		for _, j := range []int{i + d, i - d} {
			if j >= 0 && j < l.Segments() && l.cum[j+1] > l.cum[j] {
				return Bearing(l.pts[j], l.pts[j+1])
			}
		}
	}
	return 0
}

// Projection is the point of a line nearest to a position.
type Projection struct {
	Point   Point   // nearest point on the line
	Along   float64 // distance from the start of the line to Point, metres
	Offset  float64 // distance from the position to Point, metres
	Segment int     // segment holding Point
}

// Project returns the point of the line nearest to p, searching only the part
// between from and to metres along it (both clamped to the line). Limiting the
// search keeps a position on a route that passes the same street twice from
// jumping to the wrong pass.
//
// Distances are computed in a local flat projection around p, which is exact
// to centimetres over the few hundred metres a city route segment spans.
func (l *Line) Project(p Point, from, to float64) Projection {
	from = math.Max(0, math.Min(from, l.Length()))
	to = math.Max(from, math.Min(to, l.Length()))
	kx := rad(1) * earthRadius * math.Cos(rad(p.Lat)) // metres per degree of longitude at p
	ky := rad(1) * earthRadius                        // metres per degree of latitude
	local := func(q Point) (x, y float64) { return (q.Lon - p.Lon) * kx, (q.Lat - p.Lat) * ky }

	best := Projection{Offset: math.Inf(1)}
	for i := l.segmentAt(from); i < l.Segments() && l.cum[i] <= to; i++ {
		segLen := l.cum[i+1] - l.cum[i]
		ax, ay := local(l.pts[i])
		bx, by := local(l.pts[i+1])
		dx, dy := bx-ax, by-ay
		t := 0.0
		if d2 := dx*dx + dy*dy; d2 > 0 {
			t = -(ax*dx + ay*dy) / d2
		}
		if segLen > 0 { // keep the result inside [from, to]
			t = math.Max(t, (from-l.cum[i])/segLen)
			t = math.Min(t, (to-l.cum[i])/segLen)
		}
		t = math.Max(0, math.Min(t, 1))
		if off := math.Hypot(ax+t*dx, ay+t*dy); off < best.Offset {
			best = Projection{Point: lerp(l.pts[i], l.pts[i+1], t), Along: l.cum[i] + t*segLen, Offset: off, Segment: i}
		}
	}
	return best
}

func lerp(a, b Point, t float64) Point {
	return Point{Lon: a.Lon + (b.Lon-a.Lon)*t, Lat: a.Lat + (b.Lat-a.Lat)*t}
}

// Package geo holds the small geometry helpers shared by the gateway and the
// updater: bounding boxes, polyline encoding and slippy-map tile numbers.
package geo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Point is a WGS84 coordinate.
type Point struct {
	Lon float64
	Lat float64
}

// Valid reports whether the point is a finite, in-range WGS84 coordinate.
func (p Point) Valid() bool {
	return !math.IsNaN(p.Lon) && !math.IsNaN(p.Lat) &&
		!math.IsInf(p.Lon, 0) && !math.IsInf(p.Lat, 0) &&
		p.Lon >= -180 && p.Lon <= 180 && p.Lat >= -90 && p.Lat <= 90
}

// BBox is a bounding box in WGS84.
type BBox struct {
	MinLon, MinLat, MaxLon, MaxLat float64
}

// ParseBBox parses "minlon,minlat,maxlon,maxlat".
func ParseBBox(s string) (BBox, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) != 4 {
		return BBox{}, fmt.Errorf("bbox %q: want minlon,minlat,maxlon,maxlat", s)
	}
	var v [4]float64
	for i, part := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return BBox{}, fmt.Errorf("bbox %q: %w", s, err)
		}
		v[i] = f
	}
	b := BBox{MinLon: v[0], MinLat: v[1], MaxLon: v[2], MaxLat: v[3]}
	if !b.Valid() {
		return BBox{}, fmt.Errorf("bbox %q: min must be < max and coordinates within WGS84 range", s)
	}
	return b, nil
}

// Valid reports whether the box is non-degenerate and within WGS84 range.
func (b BBox) Valid() bool {
	return (Point{b.MinLon, b.MinLat}).Valid() && (Point{b.MaxLon, b.MaxLat}).Valid() &&
		b.MinLon < b.MaxLon && b.MinLat < b.MaxLat
}

// Contains reports whether p lies inside the box (edges inclusive).
func (b BBox) Contains(p Point) bool {
	return p.Valid() && p.Lon >= b.MinLon && p.Lon <= b.MaxLon && p.Lat >= b.MinLat && p.Lat <= b.MaxLat
}

// Center returns the centre of the box.
func (b BBox) Center() Point {
	return Point{Lon: (b.MinLon + b.MaxLon) / 2, Lat: (b.MinLat + b.MaxLat) / 2}
}

// String renders the box as "minlon,minlat,maxlon,maxlat".
func (b BBox) String() string {
	return fmt.Sprintf("%s,%s,%s,%s", ff(b.MinLon), ff(b.MinLat), ff(b.MaxLon), ff(b.MaxLat))
}

// Viewbox renders the box in the Nominatim "viewbox" order, which is
// left,top,right,bottom = minlon,maxlat,maxlon,minlat.
func (b BBox) Viewbox() string {
	return fmt.Sprintf("%s,%s,%s,%s", ff(b.MinLon), ff(b.MaxLat), ff(b.MaxLon), ff(b.MinLat))
}

// ParsePoint parses "lon,lat".
func ParsePoint(s string) (Point, error) {
	lonS, latS, ok := strings.Cut(strings.TrimSpace(s), ",")
	if !ok {
		return Point{}, fmt.Errorf("point %q: want lon,lat", s)
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(lonS), 64)
	if err != nil {
		return Point{}, fmt.Errorf("point %q: %w", s, err)
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(latS), 64)
	if err != nil {
		return Point{}, fmt.Errorf("point %q: %w", s, err)
	}
	p := Point{Lon: lon, Lat: lat}
	if !p.Valid() {
		return Point{}, fmt.Errorf("point %q: out of WGS84 range", s)
	}
	return p, nil
}

// Round5 rounds a coordinate to 5 decimal places (~1.1 m), the precision used
// for cache keys.
func Round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }

// FormatCoord formats a coordinate with exactly 5 decimals.
func FormatCoord(v float64) string { return strconv.FormatFloat(Round5(v), 'f', 5, 64) }

func ff(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// TileXY returns the slippy-map tile (x, y) containing p at zoom z.
func TileXY(p Point, z int) (x, y int) {
	n := math.Exp2(float64(z))
	x = int(math.Floor((p.Lon + 180) / 360 * n))
	latRad := p.Lat * math.Pi / 180
	y = int(math.Floor((1 - math.Asinh(math.Tan(latRad))/math.Pi) / 2 * n))
	maxIdx := int(n) - 1
	return clamp(x, 0, maxIdx), clamp(y, 0, maxIdx)
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// EncodePolyline encodes points with the Google polyline algorithm at the
// given precision (5 or 6 decimal digits). Points are (lon, lat) and are
// emitted in (lat, lon) order as the format requires.
func EncodePolyline(points []Point, precision int) string {
	factor := math.Pow10(precision)
	var (
		sb               strings.Builder
		prevLat, prevLon int64
	)
	for _, p := range points {
		lat := int64(math.Round(p.Lat * factor))
		lon := int64(math.Round(p.Lon * factor))
		writeVarint(&sb, lat-prevLat)
		writeVarint(&sb, lon-prevLon)
		prevLat, prevLon = lat, lon
	}
	return sb.String()
}

func writeVarint(sb *strings.Builder, v int64) {
	u := uint64(v) << 1
	if v < 0 {
		u = ^u
	}
	for u >= 0x20 {
		sb.WriteByte(byte((0x20 | (u & 0x1f)) + 63))
		u >>= 5
	}
	sb.WriteByte(byte(u + 63))
}

// DecodePolyline is the inverse of EncodePolyline.
func DecodePolyline(s string, precision int) ([]Point, error) {
	factor := math.Pow10(precision)
	var (
		pts      []Point
		lat, lon int64
		i        int
	)
	next := func() (int64, error) {
		var result uint64
		var shift uint
		for {
			if i >= len(s) {
				return 0, fmt.Errorf("polyline: truncated input")
			}
			b := uint64(s[i]) - 63
			i++
			result |= (b & 0x1f) << shift
			shift += 5
			if b < 0x20 {
				break
			}
			if shift > 60 {
				return 0, fmt.Errorf("polyline: value too large")
			}
		}
		v := int64(result >> 1)
		if result&1 != 0 {
			v = ^v
		}
		return v, nil
	}
	for i < len(s) {
		dlat, err := next()
		if err != nil {
			return nil, err
		}
		dlon, err := next()
		if err != nil {
			return nil, err
		}
		lat += dlat
		lon += dlon
		pts = append(pts, Point{Lon: float64(lon) / factor, Lat: float64(lat) / factor})
	}
	return pts, nil
}

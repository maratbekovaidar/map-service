package tracker

import (
	"fmt"

	"map-service/internal/geo"
)

// Route is the route the vehicle follows.
type Route struct {
	Version  int
	Line     *geo.Line
	Duration float64 // seconds, the router's estimate for the whole route

	// timeLeft[i] is the time from vertex i to the end of the route; nil when
	// the router gave no per-segment durations.
	timeLeft []float64
}

// NewRoute builds a Route from the router's geometry and duration.
// segDurations is nil or one duration per segment of the geometry (OSRM
// annotations); with them the remaining time follows the speed of each street,
// without them it is proportional to the remaining distance.
func NewRoute(version int, pts []geo.Point, duration float64, segDurations []float64) (*Route, error) {
	line, err := geo.NewLine(pts)
	if err != nil {
		return nil, err
	}
	r := &Route{Version: version, Line: line, Duration: duration}
	if segDurations == nil {
		return r, nil
	}
	if len(segDurations) != line.Segments() {
		return nil, fmt.Errorf("route: %d segment durations for %d segments", len(segDurations), line.Segments())
	}
	var total float64
	for _, d := range segDurations {
		total += d
	}
	if total <= 0 {
		return r, nil
	}
	// Segment durations leave out turn penalties; spread them evenly so that
	// the remaining time at the start equals the route's duration.
	scale := duration / total
	r.timeLeft = make([]float64, len(pts))
	for i := len(segDurations) - 1; i >= 0; i-- {
		r.timeLeft[i] = r.timeLeft[i+1] + segDurations[i]*scale
	}
	return r, nil
}

// Remaining returns the distance (metres) and time (seconds) left from the
// point `along` metres from the start to the end of the route.
func (r *Route) Remaining(along float64) (dist, secs float64) {
	length := r.Line.Length()
	along = max(0, min(along, length))
	dist = length - along
	if length == 0 {
		return 0, 0
	}
	if r.timeLeft == nil {
		return dist, r.Duration * dist / length
	}
	// Find the segment holding `along` and interpolate inside it.
	lo, hi := 0, r.Line.Segments()-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if r.Line.VertexAlong(mid) <= along {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	segStart, segEnd := r.Line.VertexAlong(lo), r.Line.VertexAlong(lo+1)
	frac := 0.0
	if segEnd > segStart {
		frac = (along - segStart) / (segEnd - segStart)
	}
	secs = r.timeLeft[lo+1] + (r.timeLeft[lo]-r.timeLeft[lo+1])*(1-frac)
	return dist, secs
}

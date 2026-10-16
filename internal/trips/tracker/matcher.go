package tracker

import (
	"context"
	"errors"
	"math"

	"map-service/internal/clients/osrm"
	"map-service/internal/geo"
)

// MatchAPI is the part of the OSRM client OSRMMatcher uses.
type MatchAPI interface {
	Match(ctx context.Context, p osrm.MatchParams) (*osrm.MatchResponse, error)
}

// OSRMMatcher is a Matcher backed by OSRM's match service.
type OSRMMatcher struct {
	OSRM    MatchAPI
	Profile string
}

// errTooFewFixes means the trace has fewer than two usable fixes.
var errTooFewFixes = errors.New("tracker: too few fixes to match")

// Snap implements Matcher. OSRM needs strictly increasing whole-second
// timestamps, so of several fixes within one second only the first is sent;
// the others come back unmatched, like the outliers OSRM drops.
func (m OSRMMatcher) Snap(ctx context.Context, fixes []Fix) ([]*geo.Point, error) {
	var (
		idx  []int
		p    osrm.MatchParams
		prev int64 = math.MinInt64
	)
	p.Profile = m.Profile
	for i, f := range fixes {
		ts := f.Time.Unix()
		if ts <= prev {
			continue
		}
		prev = ts
		idx = append(idx, i)
		p.Points = append(p.Points, f.Pos)
		p.Timestamps = append(p.Timestamps, ts)
		// OSRM takes the GPS standard deviation; outside 5..30 m it either
		// misses the road or considers half the neighbourhood.
		p.Radiuses = append(p.Radiuses, max(5, min(f.Accuracy, 30)))
	}
	if len(idx) < 2 {
		return nil, errTooFewFixes
	}
	res, err := m.OSRM.Match(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(res.Tracepoints) != len(idx) {
		return nil, errors.New("tracker: OSRM returned a tracepoint count that does not match the trace")
	}
	out := make([]*geo.Point, len(fixes))
	for k, tp := range res.Tracepoints {
		if tp != nil {
			out[idx[k]] = &geo.Point{Lon: tp.Location[0], Lat: tp.Location[1]}
		}
	}
	return out, nil
}

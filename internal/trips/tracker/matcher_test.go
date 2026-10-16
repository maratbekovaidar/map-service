package tracker

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"map-service/internal/clients/osrm"
	"map-service/internal/geo"
)

// fakeOSRM answers a match with one tracepoint per point, nil for the
// indexes in drop.
type fakeOSRM struct {
	got  osrm.MatchParams
	drop []int
	err  error
	n    int // tracepoints to return; -1 means one per point
}

func (f *fakeOSRM) Match(_ context.Context, p osrm.MatchParams) (*osrm.MatchResponse, error) {
	f.got = p
	if f.err != nil {
		return nil, f.err
	}
	n := len(p.Points)
	if f.n >= 0 {
		n = f.n
	}
	res := &osrm.MatchResponse{Code: "Ok", Matchings: []osrm.Matching{{Confidence: 0.9}}}
	for i := range n {
		if slices.Contains(f.drop, i) {
			res.Tracepoints = append(res.Tracepoints, nil)
			continue
		}
		pt := p.Points[i%len(p.Points)]
		res.Tracepoints = append(res.Tracepoints, &osrm.Tracepoint{Location: [2]float64{pt.Lon + 0.0001, pt.Lat}})
	}
	return res, nil
}

func TestOSRMMatcher(t *testing.T) {
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	fixes := []Fix{
		{Seq: 1, Time: at(0), Pos: geo.Point{Lon: 71.43, Lat: 51.13}, Accuracy: 0},
		{Seq: 2, Time: at(400), Pos: geo.Point{Lon: 71.4301, Lat: 51.13}, Accuracy: 12}, // same second as #1
		{Seq: 3, Time: at(1000), Pos: geo.Point{Lon: 71.4302, Lat: 51.13}, Accuracy: 80},
		{Seq: 4, Time: at(2000), Pos: geo.Point{Lon: 71.4303, Lat: 51.13}, Accuracy: 12},
	}
	api := &fakeOSRM{drop: []int{1}, n: -1}
	m := OSRMMatcher{OSRM: api, Profile: "car"}
	got, err := m.Snap(context.Background(), fixes)
	if err != nil {
		t.Fatal(err)
	}

	sec := t0.Unix()
	if api.got.Profile != "car" || len(api.got.Points) != 3 ||
		!slices.Equal(api.got.Timestamps, []int64{sec, sec + 1, sec + 2}) ||
		!slices.Equal(api.got.Radiuses, []float64{5, 30, 12}) {
		t.Errorf("request: %+v", api.got)
	}
	// Fix 2 was thinned out, fix 3 dropped by OSRM; 1 and 4 matched.
	if len(got) != 4 || got[0] == nil || got[1] != nil || got[2] != nil || got[3] == nil {
		t.Fatalf("snapped: %v", got)
	}
	if *got[3] != (geo.Point{Lon: 71.4304, Lat: 51.13}) {
		t.Errorf("fix 4 snapped to %+v", *got[3])
	}

	if _, err := m.Snap(context.Background(), fixes[:2]); !errors.Is(err, errTooFewFixes) {
		t.Errorf("two fixes in one second: %v", err)
	}
	boom := errors.New("osrm down")
	if _, err := (OSRMMatcher{OSRM: &fakeOSRM{err: boom}}).Snap(context.Background(), fixes); !errors.Is(err, boom) {
		t.Errorf("error not passed through: %v", err)
	}
	if _, err := (OSRMMatcher{OSRM: &fakeOSRM{n: 1}}).Snap(context.Background(), fixes); err == nil {
		t.Error("a tracepoint count that does not match must fail")
	}
}

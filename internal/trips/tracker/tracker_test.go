package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"map-service/internal/geo"
)

var (
	t0     = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	origin = geo.Point{Lon: 71.43, Lat: 51.13} // Astana
)

// lRoute is 1000 m east, then 800 m north; 180 s, of which the router puts
// two thirds on the first leg.
func lRoute(t *testing.T) *Route {
	t.Helper()
	corner := geo.Destination(origin, 90, 1000)
	r, err := NewRoute(1, []geo.Point{origin, corner, geo.Destination(corner, 0, 800)}, 180, []float64{100, 50})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// gen makes fixes one second apart.
type gen struct {
	seq int64
	at  time.Time
}

func newGen() *gen { return &gen{at: t0} }

func (g *gen) fix(p geo.Point, speed, heading float64) Fix {
	g.seq++
	g.at = g.at.Add(time.Second)
	return Fix{Seq: g.seq, Time: g.at, Pos: p, Accuracy: 5, Speed: speed, Heading: heading}
}

// along drives the route from metre a to metre b at speed, side metres to the
// left of it, heading along it.
func (g *gen) along(r *Route, a, b, speed, side float64) []Fix {
	var out []Fix
	for s := a; s <= b+1e-6; s += speed {
		p, brg := r.Line.PointAt(s), r.Line.BearingAt(s)
		if side != 0 {
			p = geo.Destination(p, brg-90, side)
		}
		out = append(out, g.fix(p, speed, brg))
	}
	return out
}

// straight drives n seconds from p (excluded) along bearing brg.
func (g *gen) straight(p geo.Point, brg, speed float64, n int) []Fix {
	out := make([]Fix, n)
	for i := range out {
		out[i] = g.fix(geo.Destination(p, brg, speed*float64(i+1)), speed, brg)
	}
	return out
}

// run feeds fixes to the tracker in batches of `batch`, with the server clock
// equal to the time of the last fix, and returns every update.
func run(tr *Tracker, r *Route, st *State, fixes []Fix, batch int) []Update {
	var out []Update
	for i := 0; i < len(fixes); i += batch {
		b := fixes[i:min(i+batch, len(fixes))]
		out = append(out, tr.Process(context.Background(), r, st, b, b[len(b)-1].Time))
	}
	return out
}

func reroutes(ups []Update) []*Reroute {
	var out []*Reroute
	for _, u := range ups {
		if u.Reroute != nil {
			out = append(out, u.Reroute)
		}
	}
	return out
}

// fakeMatcher snaps every fix with snap, or fails with err.
type fakeMatcher struct {
	snap  func(Fix) *geo.Point
	err   error
	calls int
}

func (m *fakeMatcher) Snap(_ context.Context, fixes []Fix) ([]*geo.Point, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	out := make([]*geo.Point, len(fixes))
	for i, f := range fixes {
		out[i] = m.snap(f)
	}
	return out, nil
}

// ontoRoute matches every fix onto the route: the GPS drifted.
func ontoRoute(r *Route) *fakeMatcher {
	return &fakeMatcher{snap: func(f Fix) *geo.Point {
		p := r.Line.Project(f.Pos, 0, r.Line.Length()).Point
		return &p
	}}
}

// whereReported matches every fix where the GPS put it: another road.
func whereReported() *fakeMatcher {
	return &fakeMatcher{snap: func(f Fix) *geo.Point { p := f.Pos; return &p }}
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func TestFollowRoute(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	tr := New(DefaultConfig(), nil)
	ups := run(tr, r, st, g.along(r, 0, 600, 12, 0), 5)

	prev := -1.0
	for _, u := range ups {
		if u.Status != OnRoute || u.Reroute != nil {
			t.Fatalf("update: %+v", u)
		}
		for _, f := range u.Fixes {
			if f.Reject != "" || f.Class != ClassOn || f.Offset > 0.5 {
				t.Fatalf("fix: %+v", f)
			}
		}
		if u.Position.Along <= prev {
			t.Fatalf("progress went from %.1f to %.1f", prev, u.Position.Along)
		}
		prev = u.Position.Along
	}
	pos := ups[len(ups)-1].Position
	// 600 m in: 400 m of the first leg (120 s after scaling) and the second leg (60 s) left.
	if !near(pos.Along, 600, 0.5) || !near(pos.RemainingDistance, 1200, 0.5) || !near(pos.RemainingDuration, 60+120*0.4, 0.2) {
		t.Errorf("position: %+v", pos)
	}
	if d := geo.Distance(pos.OnRoute, r.Line.PointAt(600)); d > 0.5 {
		t.Errorf("progress point %.2f m off", d)
	}
	if !st.Joined || st.OffRoute || st.RunFrom != nil {
		t.Errorf("state: %+v", st)
	}
}

// turnOff drives 500 m along the route, then turns right onto a side street.
func turnOff(r *Route, g *gen) (onRoute, offRoute []Fix) {
	onRoute = g.along(r, 0, 500, 10, 0)
	return onRoute, g.straight(r.Line.PointAt(500), 180, 10, 12)
}

func TestTurnOffAsksForNewRoute(t *testing.T) {
	for name, m := range map[string]*fakeMatcher{"distance only": nil, "matched": whereReported()} {
		t.Run(name, func(t *testing.T) {
			r, st, g := lRoute(t), &State{}, newGen()
			var tr *Tracker
			if m == nil {
				tr = New(DefaultConfig(), nil)
			} else {
				tr = New(DefaultConfig(), m)
			}
			on, off := turnOff(r, g)
			run(tr, r, st, on, 5)

			// One fix per batch to see exactly when it triggers.
			ups := run(tr, r, st, off, 1)
			first := -1
			for i, u := range ups {
				if u.Reroute != nil {
					first = i
					break
				}
				if u.Status != OnRoute {
					t.Fatalf("second %d: status %s before the new route", i+1, u.Status)
				}
			}
			// By distance: clearly off from 40 m (4th second), held for 3 fixes,
			// 6 s and 20 m in the 10th. The matcher can confirm the run that
			// starts near the route (2nd second) as soon as that one is held: 8th.
			want := 10
			if m != nil {
				want = 8
			}
			if first+1 != want {
				t.Fatalf("new route asked at second %d, want %d; classes: %v", first+1, want, classes(ups))
			}
			rr := ups[first].Reroute
			if rr.From != off[first].Pos || !near(rr.Heading, 180, 0.5) || rr.Reason != ReasonOffRoute {
				t.Errorf("reroute: %+v", rr)
			}
			if m != nil && (ups[first].Match != MatchOff || m.calls != 1) {
				t.Errorf("match %q after %d calls", ups[first].Match, m.calls)
			}
			// Further fixes wait for the new route without asking again.
			if u := ups[first+1]; u.Status != OffRoute || u.Reroute != nil {
				t.Errorf("next update: %+v", u)
			}
		})
	}
}

func classes(ups []Update) []Class {
	var out []Class
	for _, u := range ups {
		for _, f := range u.Fixes {
			out = append(out, f.Class)
		}
	}
	return out
}

func TestSingleSpikeIsIgnored(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	tr := New(DefaultConfig(), nil)
	fixes := g.along(r, 0, 300, 10, 0)
	fixes[15].Pos = geo.Destination(fixes[15].Pos, 0, 70) // one fix 70 m off
	ups := run(tr, r, st, fixes, 5)
	if rr := reroutes(ups); len(rr) != 0 || st.OffRoute {
		t.Errorf("a single spike caused %d new routes", len(rr))
	}
	if c := classes(ups)[15]; c != ClassOff {
		t.Errorf("spike classified %q", c)
	}
}

func TestMultipathAlongTheRoute(t *testing.T) {
	// 10 s with the GPS 45 m to the side while the car stays on the route.
	drive := func(m Matcher) ([]Update, *State) {
		r, st, g := lRoute(t), &State{}, newGen()
		tr := New(DefaultConfig(), m)
		run(tr, r, st, g.along(r, 0, 300, 10, 0), 5)
		ups := run(tr, r, st, g.along(r, 310, 400, 10, 45), 5)
		ups = append(ups, run(tr, r, st, g.along(r, 410, 500, 10, 0), 5)...)
		return ups, st
	}
	m := ontoRoute(lRoute(t))
	ups, _ := drive(m)
	if rr := reroutes(ups); len(rr) != 0 {
		t.Errorf("the matcher must veto: %d new routes", len(rr))
	}
	if m.calls == 0 {
		t.Error("the matcher was not asked")
	}

	ups, _ = drive(nil)
	if len(reroutes(ups)) != 1 {
		t.Error("without a matcher distance decides: want one new route")
	}
	ups, _ = drive(&fakeMatcher{err: errors.New("osrm down")})
	if len(reroutes(ups)) != 1 {
		t.Error("a failing matcher must leave the decision to the distance")
	}
}

func TestParallelRoadNeedsMatching(t *testing.T) {
	// 50 s on a service road 25 m beside the route: near, never clearly off.
	drive := func(m Matcher) ([]Update, *State) {
		r, st, g := lRoute(t), &State{}, newGen()
		tr := New(DefaultConfig(), m)
		run(tr, r, st, g.along(r, 0, 200, 10, 0), 5)
		return run(tr, r, st, g.along(r, 210, 700, 10, 25), 5), st
	}

	ups, st := drive(nil)
	if len(reroutes(ups)) != 0 || st.OffRoute {
		t.Error("without a matcher a near run must never become off-route")
	}
	for _, c := range classes(ups) {
		if c != ClassNear {
			t.Fatalf("classes %v", classes(ups))
		}
	}
	// Progress still follows the car.
	if !near(st.Along, 700, 1) {
		t.Errorf("progress %.1f", st.Along)
	}

	m := whereReported()
	ups, _ = drive(m)
	// The run is held after the batch ending 10 s in (fixes 210..300 m).
	if ups[0].Reroute != nil || ups[1].Match != MatchOff || ups[1].Reroute == nil || m.calls != 1 {
		t.Errorf("matched onto the service road: first batches %+v, %+v; %d calls", ups[0], ups[1], m.calls)
	}

	m = ontoRoute(lRoute(t))
	ups, st = drive(m)
	if len(reroutes(ups)) != 0 || st.OffRoute {
		t.Error("matched onto the route: no new route")
	}
	// Checked at 10 s, then not before MatchInterval (30 s): at 40 s; 50 s in all.
	if m.calls != 2 {
		t.Errorf("matcher asked %d times in 50 s, want 2", m.calls)
	}
}

func TestWrongWay(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	m := ontoRoute(r) // would veto, but must not be asked
	tr := New(DefaultConfig(), m)
	run(tr, r, st, g.along(r, 0, 500, 10, 0), 5)

	// U-turn: back along the same street.
	var back []Fix
	for s := 490.0; s >= 400; s -= 10 {
		back = append(back, g.fix(r.Line.PointAt(s), 10, 270))
	}
	ups := run(tr, r, st, back, 1)
	rr := reroutes(ups)
	if len(rr) != 1 || rr[0].Reason != ReasonWrongWay || !near(rr[0].Heading, 270, 0.5) {
		t.Fatalf("reroutes %+v, classes %v", rr, classes(ups))
	}
	if classes(ups)[0] != ClassWrongWay {
		t.Errorf("classes %v", classes(ups))
	}
	if m.calls != 0 {
		t.Errorf("matcher asked %d times for a wrong-way run", m.calls)
	}
}

func TestStartGrace(t *testing.T) {
	r := lRoute(t)
	yard := geo.Destination(origin, 180, 60) // a courtyard 60 m south of the start

	// Leaving the yard towards the route: nothing happens until it joins.
	st, g := &State{}, newGen()
	tr := New(DefaultConfig(), nil)
	fixes := []Fix{g.fix(yard, 0, -1), g.fix(yard, 0, -1)}
	fixes = append(fixes, g.straight(yard, 0, 5, 12)...) // north, through the start
	ups := run(tr, r, st, fixes, 3)
	if len(reroutes(ups)) != 0 {
		t.Fatalf("new route while leaving the yard: classes %v", classes(ups))
	}
	if cs := classes(ups); cs[0] != ClassStart || cs[len(cs)-1] == ClassStart || !st.Joined {
		t.Errorf("classes %v joined %v", cs, st.Joined)
	}

	// Leaving the yard the other way, away from the route.
	st, g = &State{}, newGen()
	ups = run(tr, r, st, g.straight(yard, 180, 8, 20), 2)
	rr := reroutes(ups)
	if len(rr) != 1 || !near(rr[0].Heading, 180, 0.5) {
		t.Fatalf("reroutes %+v, classes %v", rr, classes(ups))
	}
	// Away from the route only once out of the 100 m around its start.
	if d := geo.Distance(rr[0].From, origin); d < 100 {
		t.Errorf("asked %.0f m from the start", d)
	}
}

func TestArrival(t *testing.T) {
	r := lRoute(t)
	tr := New(DefaultConfig(), nil)

	// Driving past the destination at speed is not arriving.
	st, g := &State{}, newGen()
	ups := run(tr, r, st, g.along(r, 0, 1800, 15, 0), 10)
	if st.Arrived || ups[len(ups)-1].Status != OnRoute {
		t.Error("arrived while driving through")
	}

	// Slowing down and stopping 20 m before it is.
	st, g = &State{}, newGen()
	fixes := g.along(r, 0, 1700, 15, 0)
	fixes = append(fixes, g.fix(r.Line.PointAt(1770), 4, 0), g.fix(r.Line.PointAt(1780), 1, 0))
	ups = run(tr, r, st, fixes, 10)
	last := ups[len(ups)-1]
	if last.Status != Arrived || !st.Arrived {
		t.Fatalf("status %s", last.Status)
	}
	// Once arrived, wandering off does not ask for a route.
	ups = run(tr, r, st, g.straight(r.Line.End(), 90, 10, 15), 5)
	if len(reroutes(ups)) != 0 || ups[len(ups)-1].Status != Arrived {
		t.Error("arrival must stick")
	}
}

func TestReroutePacingAndLimit(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	cfg := DefaultConfig()
	cfg.MaxReroutes = 2
	tr := New(cfg, nil)
	on, off := turnOff(r, g)
	run(tr, r, st, on, 5)
	off = append(off, g.straight(off[len(off)-1].Pos, 180, 10, 30)...)

	// The service fails to build a route: asked again only every 15 s.
	ups := run(tr, r, st, off, 1)
	var asked []int
	for i, u := range ups {
		if u.Reroute != nil {
			asked = append(asked, i+1)
		}
	}
	if len(asked) != 3 || asked[1]-asked[0] != 15 || asked[2]-asked[1] != 15 {
		t.Fatalf("asked at seconds %v", asked)
	}

	// A new route from where the car is now; it drives off it again.
	from := ups[len(ups)-1].Position.Fix.Pos
	r2, err := NewRoute(2, []geo.Point{from, geo.Destination(from, 270, 2000)}, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Rerouted(2)
	if st.Reroutes != 1 || st.Joined || st.OffRoute || st.Along != 0 {
		t.Fatalf("state after Rerouted: %+v", st)
	}
	run(tr, r2, st, g.straight(from, 270, 10, 20), 5)
	ups = run(tr, r2, st, g.straight(geo.Destination(from, 270, 200), 0, 10, 15), 5)
	if len(reroutes(ups)) != 1 {
		t.Fatal("second departure: want a new route")
	}
	st.Rerouted(3)
	r3, err := NewRoute(3, []geo.Point{st.last().Pos, geo.Destination(st.last().Pos, 270, 2000)}, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := st.last().Pos
	run(tr, r3, st, g.straight(p, 270, 10, 20), 5)
	ups = run(tr, r3, st, g.straight(geo.Destination(p, 270, 200), 0, 10, 30), 5)
	if len(reroutes(ups)) != 0 || ups[len(ups)-1].Status != OffRoute {
		t.Error("past MaxReroutes the trip stays off-route without asking")
	}
}

func TestFixFiltering(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	tr := New(DefaultConfig(), nil)
	f := g.along(r, 0, 60, 10, 0) // seq 1..7

	bad := f[3]
	bad.Accuracy = 80
	invalid := f[4]
	invalid.Pos = geo.Point{Lon: 500}
	stale := f[5]
	stale.Time = f[1].Time // older than an accepted fix
	batch := []Fix{f[2], f[0], f[1], f[0], bad, invalid, stale}
	u := tr.Process(context.Background(), r, st, batch, f[2].Time)
	want := []struct {
		seq    int64
		reject string
	}{{1, ""}, {1, RejectDuplicate}, {2, ""}, {3, ""}, {4, RejectInaccurate}, {5, RejectInvalid}, {6, RejectOutOfOrder}}
	if len(u.Fixes) != len(want) {
		t.Fatalf("results %+v", u.Fixes)
	}
	for i, w := range want {
		if u.Fixes[i].Seq != w.seq || u.Fixes[i].Reject != w.reject {
			t.Errorf("result %d = %+v, want %+v", i, u.Fixes[i], w)
		}
	}
	// A fix reported again in a later batch is a duplicate too.
	if u := tr.Process(context.Background(), r, st, []Fix{f[2]}, f[2].Time); u.Fixes[0].Reject != RejectDuplicate {
		t.Errorf("replayed fix: %+v", u.Fixes[0])
	}

	// A jump of 2 km in a second is ignored, and the next good fix accepted.
	jump := g.fix(geo.Destination(f[6].Pos, 0, 2000), 10, 90)
	good := g.fix(r.Line.PointAt(80), 10, 90)
	u = tr.Process(context.Background(), r, st, []Fix{jump, good}, good.Time)
	if u.Fixes[0].Reject != RejectJump || u.Fixes[1].Reject != "" || u.Position.Fix.Seq != good.Seq {
		t.Errorf("jump: %+v", u.Fixes)
	}
}

func TestBadFirstFixIsReplaced(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	tr := New(DefaultConfig(), nil)
	coldStart := g.fix(geo.Destination(origin, 200, 3000), 0, -1) // 3 km off
	fixes := append([]Fix{coldStart}, g.along(r, 0, 100, 10, 0)...)
	ups := run(tr, r, st, fixes, 20)
	got := ups[0].Fixes
	if got[0].Reject != "" || got[1].Reject != RejectJump || got[2].Reject != "" {
		t.Fatalf("results %+v", got[:3])
	}
	// The car is followed from the second fix on and was never off the route.
	if !near(st.Along, 100, 0.5) || len(reroutes(ups)) != 0 || st.OffRoute {
		t.Errorf("along %.1f offRoute %v", st.Along, st.OffRoute)
	}
}

func TestMissingSpeedAndHeadingAreDerived(t *testing.T) {
	r, st, g := lRoute(t), &State{}, newGen()
	tr := New(DefaultConfig(), nil)
	fixes := g.along(r, 0, 100, 12, 0)
	for i := range fixes {
		fixes[i].Speed, fixes[i].Heading = -1, -1
	}
	run(tr, r, st, fixes, 5)
	last := st.last()
	if !near(last.Speed, 12, 0.05) || !near(last.Heading, 90, 0.5) {
		t.Errorf("derived speed %.2f heading %.2f", last.Speed, last.Heading)
	}
}

func TestRouteThatDoublesBack(t *testing.T) {
	// Out 400 m and back on a lane 8 m away: closer than OnTolerance, so only
	// the window keeps progress on the right pass.
	e := geo.Destination(origin, 90, 400)
	r, err := NewRoute(1, []geo.Point{origin, e, geo.Destination(e, 0, 8), geo.Destination(origin, 0, 8)}, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, g := &State{}, newGen()
	ups := run(New(DefaultConfig(), nil), r, st, g.along(r, 0, r.Line.Length(), 8, 0), 3)
	prev := -1.0
	for _, u := range ups {
		if u.Position.Along < prev {
			t.Fatalf("progress went back from %.1f to %.1f", prev, u.Position.Along)
		}
		prev = u.Position.Along
		for _, f := range u.Fixes {
			if f.Offset > 0.5 {
				t.Fatalf("fix %d projected %.1f m away (along %.1f)", f.Seq, f.Offset, f.Along)
			}
		}
	}
	if len(reroutes(ups)) != 0 {
		t.Error("no new route on a route that doubles back")
	}
}

func TestBackgroundGap(t *testing.T) {
	// The app was in the background for a minute and sends everything at once.
	r := lRoute(t)
	tr := New(DefaultConfig(), nil)

	// A detour that came back to the route: nothing to do.
	st, g := &State{}, newGen()
	run(tr, r, st, g.along(r, 0, 200, 10, 0), 5)
	detour := g.straight(r.Line.PointAt(200), 180, 10, 15)
	detour = append(detour, g.straight(detour[len(detour)-1].Pos, 90, 10, 10)...)
	detour = append(detour, g.straight(detour[len(detour)-1].Pos, 0, 10, 15)...)
	detour = append(detour, g.along(r, 310, 500, 10, 0)...)
	u := tr.Process(context.Background(), r, st, detour, detour[len(detour)-1].Time)
	if u.Reroute != nil || u.Status != OnRoute || !near(st.Along, 500, 0.5) {
		t.Errorf("detour: %+v along %.1f", u.Reroute, st.Along)
	}

	// Gone for good: one new route, from the latest fix.
	st, g = &State{}, newGen()
	run(tr, r, st, g.along(r, 0, 200, 10, 0), 5)
	gone := g.straight(r.Line.PointAt(200), 180, 10, 60)
	u = tr.Process(context.Background(), r, st, gone, gone[len(gone)-1].Time)
	if u.Reroute == nil || u.Reroute.From != gone[len(gone)-1].Pos {
		t.Errorf("gone: %+v", u.Reroute)
	}
}

func TestStateSurvivesJSON(t *testing.T) {
	// Storing the state between batches must not change any decision.
	r := lRoute(t)
	tr := New(DefaultConfig(), whereReported())
	on, off := turnOff(r, newGen())
	fixes := append(on, off...)

	direct := run(tr, r, &State{}, fixes, 3)

	var stored []byte
	var viaJSON []Update
	for i := 0; i < len(fixes); i += 3 {
		var st State
		if stored != nil {
			if err := json.Unmarshal(stored, &st); err != nil {
				t.Fatal(err)
			}
		}
		b := fixes[i:min(i+3, len(fixes))]
		viaJSON = append(viaJSON, tr.Process(context.Background(), r, &st, b, b[len(b)-1].Time))
		var err error
		if stored, err = json.Marshal(st); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := json.Marshal(direct)
	b, _ := json.Marshal(viaJSON)
	if string(a) != string(b) {
		t.Errorf("updates differ after a JSON round trip of the state")
	}
	if len(reroutes(direct)) != 1 {
		t.Errorf("want one new route, got %d", len(reroutes(direct)))
	}
}

func TestRemaining(t *testing.T) {
	r := lRoute(t)
	for _, c := range []struct{ along, dist, secs float64 }{
		{-10, 1800, 180}, {0, 1800, 180}, {500, 1300, 120}, {1000, 800, 60}, {1400, 400, 30}, {1800, 0, 0}, {5000, 0, 0},
	} {
		d, s := r.Remaining(c.along)
		if !near(d, c.dist, 0.01) || !near(s, c.secs, 0.01) {
			t.Errorf("Remaining(%v) = %.2f m, %.2f s; want %v, %v", c.along, d, s, c.dist, c.secs)
		}
	}

	// Without per-segment durations time follows distance.
	plain, err := NewRoute(1, r.Line.Points(), 180, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, s := plain.Remaining(900); !near(s, 90, 0.01) {
		t.Errorf("proportional remaining time %.2f", s)
	}
	if _, err := NewRoute(1, r.Line.Points(), 180, []float64{1}); err == nil {
		t.Error("a duration count that does not match the segments must be rejected")
	}
}

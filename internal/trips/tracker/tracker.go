// Package tracker follows a vehicle along its route. From the GPS fixes the
// vehicle reports it keeps the progress along the route and the remaining
// distance and time, notices that the vehicle has left the route (and says
// when to ask for a new one) and that it has arrived.
//
// A Tracker holds no per-trip data: everything it needs between batches of
// fixes lives in State, which the caller stores with the trip. Its only I/O is
// an optional Matcher (OSRM map matching), asked at most once per batch.
package tracker

import (
	"context"
	"slices"
	"time"

	"map-service/internal/geo"
)

// Fix is one GPS position reported by the vehicle.
type Fix struct {
	Seq      int64     `json:"seq"` // per trip, from 1, increasing
	Time     time.Time `json:"time"`
	Pos      geo.Point `json:"pos"`
	Accuracy float64   `json:"accuracy"` // metres; 0 means unknown
	Speed    float64   `json:"speed"`    // m/s; negative means unknown
	Heading  float64   `json:"heading"`  // degrees clockwise from north; negative means unknown
}

// Class is where an accepted fix lies relative to the route.
type Class string

// Fix classes.
const (
	ClassOn       Class = "on"        // on the route
	ClassNear     Class = "near"      // too far to be sure it is on, too close to call it off
	ClassOff      Class = "off"       // clearly away from the route
	ClassWrongWay Class = "wrong_way" // on the route, driving against it
	ClassStart    Class = "start"     // not on the route yet, but still close to its start
)

// Reasons a fix is ignored.
const (
	RejectDuplicate  = "duplicate"    // its seq was already seen
	RejectOutOfOrder = "out_of_order" // not newer than the last accepted fix
	RejectInvalid    = "invalid"      // not a coordinate
	RejectInaccurate = "inaccurate"   // accuracy worse than Config.MaxAccuracy
	RejectJump       = "jump"         // implies an impossible speed
)

// Status is the state of the trip after a batch.
type Status string

// Trip statuses.
const (
	OnRoute  Status = "on_route"
	OffRoute Status = "off_route"
	Arrived  Status = "arrived"
)

// Reasons for a new route.
const (
	ReasonOffRoute = "off_route" // the vehicle took another road
	ReasonWrongWay = "wrong_way" // the vehicle drives against the route
)

// Match verdicts, reported in Update.Match.
const (
	MatchOff     = "off"     // matched onto another road
	MatchOn      = "on"      // matched onto the route: the GPS drifted
	MatchUnknown = "unknown" // the matcher failed or was inconclusive
)

// Config holds the thresholds. DefaultConfig has values for a car in a city.
type Config struct {
	// Fix filtering.
	MaxAccuracy     float64 // metres; less accurate fixes are ignored
	DefaultAccuracy float64 // metres, assumed for fixes without accuracy
	MaxSpeed        float64 // m/s; a faster move between fixes is a GPS jump
	JumpSlack       float64 // metres a fix may jump on top of MaxSpeed

	// Where a fix lies.
	OnTolerance      float64 // metres from the route that still count as on it
	OffDistance      float64 // metres from the route that are clearly off it...
	AccuracyFactor   float64 // ...or this many times the fix accuracy, if more
	WrongWayAngle    float64 // degrees between heading and route that mean "against it"
	WrongWayMinSpeed float64 // m/s; below it the heading is too noisy to judge
	StartGrace       float64 // metres around the start where a vehicle that has not joined the route yet is left alone

	// Deciding that the vehicle has left the route.
	ConfirmFixes  int           // consecutive fixes away from the route...
	ConfirmTime   time.Duration // ...spanning at least this long...
	ConfirmMove   float64       // ...and this many metres of movement
	SnapTolerance float64       // metres; a fix matched farther from the route is on another road
	MatchFixes    int           // recent fixes kept and sent to the matcher
	MatchInterval time.Duration // between matcher checks of a vehicle staying near the route

	// Following the route.
	LookBack  float64 // metres behind the progress a fix may be projected to
	LookAhead float64 // metres ahead of it, plus MaxSpeed times the time since

	// Arrival.
	ArriveRadius   float64 // metres from the destination
	ArriveMaxSpeed float64 // m/s

	// New routes.
	RerouteInterval time.Duration // between requests for a new route
	MaxReroutes     int           // per trip
	HeadingMinSpeed float64       // m/s; slower vehicles get a new route without a heading
}

// DefaultConfig returns thresholds for a car in a city. They are a starting
// point to be tuned on recorded trips.
func DefaultConfig() Config {
	return Config{
		MaxAccuracy: 50, DefaultAccuracy: 15, MaxSpeed: 60, JumpSlack: 100,
		OnTolerance: 15, OffDistance: 35, AccuracyFactor: 1.5,
		WrongWayAngle: 120, WrongWayMinSpeed: 3, StartGrace: 100,
		ConfirmFixes: 3, ConfirmTime: 6 * time.Second, ConfirmMove: 20,
		SnapTolerance: 8, MatchFixes: 10, MatchInterval: 30 * time.Second,
		LookBack: 50, LookAhead: 300,
		ArriveRadius: 40, ArriveMaxSpeed: 2,
		RerouteInterval: 15 * time.Second, MaxReroutes: 30, HeadingMinSpeed: 2,
	}
}

// Matcher snaps recent fixes onto the road network: one entry per fix, nil
// where a fix could not be matched.
type Matcher interface {
	Snap(ctx context.Context, fixes []Fix) ([]*geo.Point, error)
}

// Tracker follows vehicles along their routes. It is safe for concurrent use
// as long as each State is used by one goroutine at a time.
type Tracker struct {
	cfg     Config
	matcher Matcher
}

// New returns a Tracker; matcher may be nil, then a vehicle staying near the
// route is never declared off it, and a vehicle clearly away from it is
// judged by distance alone.
func New(cfg Config, matcher Matcher) *Tracker {
	return &Tracker{cfg: cfg, matcher: matcher}
}

// State is everything the tracker remembers about one trip between batches.
// The zero State is the state of a new trip. It is meant to be stored as JSON.
type State struct {
	RouteVersion int       `json:"route_version"`
	Joined       bool      `json:"joined"`   // the vehicle has been on the current route
	Along        float64   `json:"along"`    // progress, metres from the route start; never decreases
	AlongAt      time.Time `json:"along_at"` // time of the last fix on or near the route

	LastSeq int64 `json:"last_seq"`
	Recent  []Fix `json:"recent"`            // last accepted fixes, oldest first
	Suspect *Fix  `json:"suspect,omitempty"` // last fix rejected as a jump

	// The current run of consecutive fixes that are not on the route.
	RunFrom  *Fix `json:"run_from,omitempty"` // its first fix
	RunFixes int  `json:"run_fixes"`
	OffFrom  *Fix `json:"off_from,omitempty"` // its first clearly-off (or wrong-way) fix
	OffFixes int  `json:"off_fixes"`
	WrongWay bool `json:"wrong_way"` // the latest clearly-off fix was a wrong-way one

	MatchedAt time.Time `json:"matched_at"` // last matcher check of a run near the route
	OffRoute  bool      `json:"off_route"`  // the vehicle has left the route
	Arrived   bool      `json:"arrived"`

	Reroutes       int       `json:"reroutes"`         // new routes so far
	RerouteAskedAt time.Time `json:"reroute_asked_at"` // last time Update.Reroute was set
}

func (st *State) last() *Fix {
	if len(st.Recent) == 0 {
		return nil
	}
	return &st.Recent[len(st.Recent)-1]
}

func (st *State) clearRun() {
	st.RunFrom, st.RunFixes, st.OffFrom, st.OffFixes, st.WrongWay = nil, 0, nil, 0, false
}

// switchRoute starts following another route; the fixes and counters stay.
func (st *State) switchRoute(version int) {
	st.RouteVersion = version
	st.Joined, st.Along, st.AlongAt = false, 0, time.Time{}
	st.clearRun()
	st.OffRoute, st.MatchedAt = false, time.Time{}
}

// Rerouted records that a new route (version) was built after Update.Reroute
// asked for one. It counts towards Config.MaxReroutes; the next Process call
// follows the new route.
func (st *State) Rerouted(version int) {
	st.Reroutes++
	st.switchRoute(version)
}

// FixResult is what happened to one fix.
type FixResult struct {
	Seq    int64   `json:"seq"`
	Reject string  `json:"reject,omitempty"` // why the fix was ignored; empty if accepted
	Class  Class   `json:"class,omitempty"`
	Along  float64 `json:"along"`  // where it projects onto the route, metres from the start
	Offset float64 `json:"offset"` // how far from the route, metres
}

// Position is where the vehicle is after a batch.
type Position struct {
	Fix               Fix       // last accepted fix
	OnRoute           geo.Point // the progress point on the route
	Along             float64   // metres from the route start
	RemainingDistance float64   // metres
	RemainingDuration float64   // seconds
}

// Reroute asks for a new route from the vehicle to the destination.
type Reroute struct {
	From    geo.Point
	Heading float64 // degrees, to make the route start in the direction of travel; negative when unknown
	Reason  string  // ReasonOffRoute or ReasonWrongWay
}

// Update is the result of one batch.
type Update struct {
	Fixes    []FixResult // one per input fix, in seq order
	Status   Status
	Position *Position // nil until a fix is accepted
	Reroute  *Reroute  // set when a new route should be requested now
	Match    string    // the matcher's verdict if it was asked in this batch
}

// Process feeds a batch of fixes for route r into st and reports the result.
// Fixes may come in any order and repeat earlier ones. now is the server
// time, which paces matcher checks and new routes; durations between fixes
// use the device clock of the fixes.
func (t *Tracker) Process(ctx context.Context, r *Route, st *State, fixes []Fix, now time.Time) Update {
	if st.RouteVersion != r.Version {
		st.switchRoute(r.Version)
	}
	sorted := slices.Clone(fixes)
	slices.SortStableFunc(sorted, func(a, b Fix) int { return compareInt(a.Seq, b.Seq) })

	upd := Update{Fixes: make([]FixResult, 0, len(sorted))}
	for _, f := range sorted {
		upd.Fixes = append(upd.Fixes, t.step(r, st, f))
	}
	if !st.Arrived && !st.OffRoute {
		upd.Match = t.confirm(ctx, r, st, now)
	}
	upd.Status, upd.Reroute = t.decide(st, now)
	if last := st.last(); last != nil {
		dist, secs := r.Remaining(st.Along)
		upd.Position = &Position{
			Fix: *last, OnRoute: r.Line.PointAt(st.Along), Along: st.Along,
			RemainingDistance: dist, RemainingDuration: secs,
		}
	}
	return upd
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// step takes one fix in.
func (t *Tracker) step(r *Route, st *State, f Fix) FixResult {
	res := FixResult{Seq: f.Seq}
	if res.Reject = t.admit(st, &f); res.Reject != "" {
		return res
	}
	st.Recent = append(st.Recent, f)
	if keep := max(t.cfg.MatchFixes, 2); len(st.Recent) > keep {
		st.Recent = slices.Clone(st.Recent[len(st.Recent)-keep:])
	}

	from, to := t.window(r, st, f.Time)
	pr := r.Line.Project(f.Pos, from, to)
	res.Along, res.Offset = pr.Along, pr.Offset
	res.Class = t.classify(r, st, f, pr)
	if !st.Arrived && t.arrived(r, f, pr, res.Class) {
		st.Arrived = true
	}

	switch res.Class {
	case ClassOn:
		st.Joined, st.OffRoute = true, false
		st.clearRun()
		st.advance(pr.Along, f.Time)
	case ClassNear:
		st.advance(pr.Along, f.Time)
		st.extendRun(f)
	case ClassOff, ClassWrongWay:
		st.extendRun(f)
		if st.OffFrom == nil {
			st.OffFrom = &f
		}
		st.OffFixes++
		st.WrongWay = res.Class == ClassWrongWay
	case ClassStart: // waiting for the vehicle to reach the route
	}
	return res
}

func (st *State) advance(along float64, at time.Time) {
	st.Along = max(st.Along, along)
	st.AlongAt = at
}

func (st *State) extendRun(f Fix) {
	if st.RunFrom == nil {
		st.RunFrom = &f
	}
	st.RunFixes++
}

// admit filters a fix and fills in its speed and heading from the previous
// one when the device did not report them. It returns a Reject* reason, or
// "" for an accepted fix.
func (t *Tracker) admit(st *State, f *Fix) string {
	if f.Seq <= st.LastSeq {
		return RejectDuplicate
	}
	st.LastSeq = f.Seq
	switch {
	case !f.Pos.Valid():
		return RejectInvalid
	case f.Accuracy > t.cfg.MaxAccuracy:
		return RejectInaccurate
	}
	prev := st.last()
	if prev != nil && !f.Time.After(prev.Time) {
		return RejectOutOfOrder
	}
	if prev != nil && t.jump(*prev, *f) {
		// Two consistent fixes far from the last accepted one mean that one
		// was the outlier (typically the first fix of a cold GPS): restart
		// from them.
		if s := st.Suspect; s != nil && f.Time.After(s.Time) && !t.jump(*s, *f) {
			st.Recent = []Fix{*s}
			prev = &st.Recent[0]
		} else {
			cp := *f
			st.Suspect = &cp
			return RejectJump
		}
	}
	st.Suspect = nil
	if prev != nil {
		d := geo.Distance(prev.Pos, f.Pos)
		if dt := f.Time.Sub(prev.Time).Seconds(); f.Speed < 0 && dt > 0 && dt <= 30 {
			f.Speed = d / dt
		}
		if f.Heading < 0 && d >= 10 {
			f.Heading = geo.Bearing(prev.Pos, f.Pos)
		}
	}
	return ""
}

func (t *Tracker) jump(a, b Fix) bool {
	dt := b.Time.Sub(a.Time).Seconds()
	return geo.Distance(a.Pos, b.Pos) > t.cfg.JumpSlack+t.cfg.MaxSpeed*dt
}

// window is the part of the route a fix taken at `at` may project to: around
// the progress, reaching as far ahead as the vehicle could have driven since
// it was last seen on the route. Before that, the whole route.
func (t *Tracker) window(r *Route, st *State, at time.Time) (from, to float64) {
	if st.AlongAt.IsZero() {
		return 0, r.Line.Length()
	}
	dt := max(0, at.Sub(st.AlongAt).Seconds())
	return st.Along - t.cfg.LookBack, st.Along + t.cfg.LookAhead + t.cfg.MaxSpeed*dt
}

func (t *Tracker) classify(r *Route, st *State, f Fix, pr geo.Projection) Class {
	acc := f.Accuracy
	if acc <= 0 {
		acc = t.cfg.DefaultAccuracy
	}
	offTol := max(t.cfg.OffDistance, t.cfg.AccuracyFactor*acc)
	wrongWay := pr.Offset <= offTol && f.Speed >= t.cfg.WrongWayMinSpeed && f.Heading >= 0 &&
		geo.AngleDiff(f.Heading, r.Line.BearingAt(pr.Along)) > t.cfg.WrongWayAngle

	var c Class
	switch {
	case wrongWay:
		c = ClassWrongWay
	case pr.Offset <= t.cfg.OnTolerance:
		c = ClassOn
	case pr.Offset <= offTol:
		c = ClassNear
	default:
		c = ClassOff
	}
	if c != ClassOn && !st.Joined && geo.Distance(f.Pos, r.Line.Start()) <= t.cfg.StartGrace {
		c = ClassStart
	}
	return c
}

func (t *Tracker) arrived(r *Route, f Fix, pr geo.Projection, c Class) bool {
	atEnd := geo.Distance(f.Pos, r.Line.End()) <= t.cfg.ArriveRadius ||
		(c == ClassOn || c == ClassNear) && r.Line.Length()-pr.Along <= t.cfg.ArriveRadius
	slow := f.Speed < 0 || f.Speed <= t.cfg.ArriveMaxSpeed
	return atEnd && slow
}

// confirm decides at the end of a batch whether the current run away from
// the route means the vehicle has left it. It returns the matcher's verdict
// if the matcher was asked.
func (t *Tracker) confirm(ctx context.Context, r *Route, st *State, now time.Time) string {
	last := st.last()
	if last == nil || st.RunFrom == nil {
		return ""
	}
	held := func(from *Fix, fixes int) bool {
		return fixes >= t.cfg.ConfirmFixes && last.Time.Sub(from.Time) >= t.cfg.ConfirmTime &&
			geo.Distance(from.Pos, last.Pos) >= t.cfg.ConfirmMove
	}

	// Clearly away from the route. Matching only guards against GPS
	// multipath between tall buildings; driving against the route on a
	// two-way street matches onto the route itself, so it is not asked.
	if st.OffFrom != nil && held(st.OffFrom, st.OffFixes) {
		if st.WrongWay || t.matcher == nil {
			st.OffRoute = true
			return ""
		}
		v := t.matchedOff(ctx, r, st)
		if v == MatchOn {
			st.clearRun()
		} else { // a failed match leaves the decision to the distance
			st.OffRoute = true
		}
		return v
	}

	// Staying near the route: a parallel service road or a GPS offset.
	// Only matching can tell, and it is not asked more often than
	// MatchInterval.
	if t.matcher != nil && held(st.RunFrom, st.RunFixes) && now.Sub(st.MatchedAt) >= t.cfg.MatchInterval {
		st.MatchedAt = now
		v := t.matchedOff(ctx, r, st)
		switch v {
		case MatchOff:
			st.OffRoute = true
		case MatchOn:
			st.clearRun()
		}
		return v
	}
	return ""
}

// matchedOff snaps the recent fixes onto the road network and looks at the
// last two of the current run that matched: both on another road mean the
// vehicle left the route, both on the route mean the GPS drifted.
func (t *Tracker) matchedOff(ctx context.Context, r *Route, st *State) string {
	snapped, err := t.matcher.Snap(ctx, st.Recent)
	if err != nil || len(snapped) != len(st.Recent) {
		return MatchUnknown
	}
	var votes []bool
	for i := len(st.Recent) - 1; i >= 0 && len(votes) < 2; i-- {
		f := st.Recent[i]
		if f.Seq < st.RunFrom.Seq {
			break
		}
		if snapped[i] == nil {
			continue
		}
		from, to := t.window(r, st, f.Time)
		votes = append(votes, r.Line.Project(*snapped[i], from, to).Offset > t.cfg.SnapTolerance)
	}
	switch {
	case len(votes) < 2 || votes[0] != votes[1]:
		return MatchUnknown
	case votes[0]:
		return MatchOff
	}
	return MatchOn
}

// decide turns the state into the batch status and, when the vehicle is off
// the route and the pacing allows, a request for a new route.
func (t *Tracker) decide(st *State, now time.Time) (Status, *Reroute) {
	switch {
	case st.Arrived:
		return Arrived, nil
	case !st.OffRoute:
		return OnRoute, nil
	case st.Reroutes >= t.cfg.MaxReroutes,
		!st.RerouteAskedAt.IsZero() && now.Sub(st.RerouteAskedAt) < t.cfg.RerouteInterval:
		return OffRoute, nil
	}
	st.RerouteAskedAt = now
	last := st.last()
	rr := &Reroute{From: last.Pos, Heading: -1, Reason: ReasonOffRoute}
	if last.Heading >= 0 && last.Speed >= t.cfg.HeadingMinSpeed {
		rr.Heading = last.Heading
	}
	if st.WrongWay {
		rr.Reason = ReasonWrongWay
	}
	return OffRoute, rr
}

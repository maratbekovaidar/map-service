package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"map-service/internal/clients/osrm"
	"map-service/internal/config"
	"map-service/internal/geo"
	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// Most tests need PostgreSQL, like the store's: `make test-trips-db`.
const dsnEnv = "TRIPS_TEST_DATABASE_URL"

const testKey = "test-key"

var (
	t0     = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	origin = geo.Point{Lon: 71.4306, Lat: 51.1283} // Bayterek
	dest   = geo.Point{Lon: 71.4700, Lat: 51.1500}
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeRouter answers with an L-shaped route: along the parallel of the
// start, then along the meridian of the end, at 10 m/s.
type fakeRouter struct {
	mu        sync.Mutex
	calls     []osrm.RouteParams
	err       error
	healthErr error
}

func (f *fakeRouter) Route(_ context.Context, p osrm.RouteParams) (*osrm.RouteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	if f.err != nil {
		return nil, f.err
	}
	from, to := p.Points[0], p.Points[1]
	corner := geo.Point{Lon: to.Lon, Lat: from.Lat}
	d1, d2 := geo.Distance(from, corner), geo.Distance(corner, to)
	return &osrm.RouteResponse{Code: "Ok", Routes: []osrm.Route{{
		Distance: d1 + d2, Duration: (d1 + d2) / 10,
		Geometry: osrm.RouteGeometry{Encoded: geo.EncodePolyline([]geo.Point{from, corner, to}, 6)},
		Legs:     []osrm.Leg{{Annotation: &osrm.Annotation{Duration: []float64{d1 / 10, d2 / 10}}}},
	}}}, nil
}

func (f *fakeRouter) Health(context.Context, geo.Point) error { return f.healthErr }

func (f *fakeRouter) set(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeRouter) last() osrm.RouteParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type harness struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	store  *store.Store
	router *fakeRouter
	clock  *clock
	logs   *syncBuffer
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make test-trips-db`", dsnEnv)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "test_api_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := store.Open(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	// The simulated app sends its batches back to back on a stopped clock,
	// so the per-trip limit is off unless a test turns it on.
	vals := map[string]string{"API_KEYS": testKey, "TRIPS_RATE_LIMIT_TRIP_RPS": "0"}
	for k, v := range env {
		vals[k] = v
	}
	cfg, err := config.LoadTrips(func(k string) string { return vals[k] })
	if err != nil {
		t.Fatal(err)
	}
	st := testStore(t)
	clk := &clock{t: t0}
	st.SetClock(clk.now)
	logs := &syncBuffer{}
	router := &fakeRouter{}
	srv := New(Deps{
		Cfg: cfg, Log: slog.New(slog.NewJSONHandler(logs, nil)), Store: st, Router: router,
		Tracker: tracker.New(tracker.DefaultConfig(), nil), Version: func() string { return "v-test" }, Now: clk.now,
	})
	return &harness{t: t, srv: srv, h: srv.Handler(), store: st, router: router, clock: clk, logs: logs}
}

// do sends a request; body is marshalled to JSON unless it is a string.
func (hs *harness) do(method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	hs.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		js, err := json.Marshal(b)
		if err != nil {
			hs.t.Fatal(err)
		}
		rd = bytes.NewReader(js)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "198.51.100.7:4000"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) httpkit.ErrorDetail {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	e := decode[httpkit.ErrorBody](t, rec).Error
	if e.Code != code {
		t.Fatalf("error code %q, want %q (%s)", e.Code, code, e.Message)
	}
	return e
}

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

// createTrip creates the standard trip.
func (hs *harness) createTrip() CreateTripResponse {
	hs.t.Helper()
	rec := hs.do("POST", "/v1/trips", map[string]any{
		"origin": LatLon{Lat: origin.Lat, Lon: origin.Lon}, "destination": LatLon{Lat: dest.Lat, Lon: dest.Lon},
	}, "X-API-Key", testKey)
	if rec.Code != http.StatusCreated {
		hs.t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	return decode[CreateTripResponse](hs.t, rec)
}

// driver simulates the app: fixes 1 s apart along a line, on the shared
// clock, sent in batches.
type driverSim struct {
	hs    *harness
	trip  CreateTripResponse
	seq   int64
	known int
}

func (d *driverSim) fix(p geo.Point, speed, heading float64) PositionIn {
	d.seq++
	d.hs.clock.add(time.Second)
	acc := 5.0
	return PositionIn{Seq: d.seq, TS: d.hs.clock.now(), Lat: &p.Lat, Lon: &p.Lon, Accuracy: &acc, Speed: &speed, Heading: &heading}
}

func (d *driverSim) along(line *geo.Line, from, to, speed float64) []PositionIn {
	var out []PositionIn
	for s := from; s <= to+1e-6; s += speed {
		out = append(out, d.fix(line.PointAt(s), speed, line.BearingAt(s)))
	}
	return out
}

func (d *driverSim) send(ps []PositionIn) *httptest.ResponseRecorder {
	return d.hs.do("POST", "/v1/trips/"+d.trip.Trip.ID+"/positions",
		PositionsRequest{KnownRouteVersion: d.known, Positions: ps}, bearer(d.trip.DriverToken)...)
}

// drive sends ps in batches of 4 and returns the last answer.
func (d *driverSim) drive(ps []PositionIn) PositionsResponse {
	d.hs.t.Helper()
	var last PositionsResponse
	for i := 0; i < len(ps); i += 4 {
		rec := d.send(ps[i:min(i+4, len(ps))])
		if rec.Code != http.StatusOK {
			d.hs.t.Fatalf("positions: %d %s", rec.Code, rec.Body.String())
		}
		last = decode[PositionsResponse](d.hs.t, rec)
		if last.Route != nil {
			d.known = last.Route.Version
		}
	}
	return last
}

func lineOf(t *testing.T, r RouteDTO) *geo.Line {
	t.Helper()
	pts, err := geo.DecodePolyline(r.Geometry, 6)
	if err != nil {
		t.Fatal(err)
	}
	l, err := geo.NewLine(pts)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// mangle changes the last character of a token.
func mangle(tok string) string {
	last := byte('A')
	if tok[len(tok)-1] == 'A' {
		last = 'Q'
	}
	return tok[:len(tok)-1] + string(last)
}

func TestCreateTrip(t *testing.T) {
	hs := newHarness(t, nil)
	resp := hs.createTrip()
	if !strings.HasPrefix(resp.DriverToken, "drv_") || !strings.HasPrefix(resp.WatchToken, "wch_") || len(resp.DriverToken) != 4+43 {
		t.Errorf("tokens %q %q", resp.DriverToken, resp.WatchToken)
	}
	if resp.Trip.Status != "active" || resp.Trip.Origin.Lat != origin.Lat || !resp.Trip.CreatedAt.Equal(t0) || resp.Trip.EndedAt != nil {
		t.Errorf("trip %+v", resp.Trip)
	}
	r := resp.Route
	if r.Version != 1 || r.Reason != "initial" || r.Distance < 4000 || r.Duration != r.Distance/10 {
		t.Errorf("route %+v", r)
	}
	if l := lineOf(t, r); geo.Distance(l.Start(), origin) > 0.5 || geo.Distance(l.End(), dest) > 0.5 {
		t.Errorf("geometry from %v to %v", l.Start(), l.End())
	}
	p := hs.router.last()
	if p.Geometry != osrm.Polyline6 || !p.Annotations || p.Bearings != nil || p.Profile != "car" {
		t.Errorf("router asked %+v", p)
	}
	// The tokens are stored only as hashes.
	trip, err := hs.store.Trip(context.Background(), resp.Trip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(trip.DriverTokenHash, []byte(resp.DriverToken)) || !bytes.Equal(trip.DriverTokenHash, hashToken(resp.DriverToken)) {
		t.Error("driver token not stored as its hash")
	}
	if rec := hs.do("POST", "/v1/trips", nil); rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("answers must not be cached")
	}

	// A heading makes the route start that way.
	rec := hs.do("POST", "/v1/trips", map[string]any{"origin": latLon(origin), "destination": latLon(dest), "heading": 271.6},
		"X-API-Key", testKey)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if b := hs.router.last().Bearings; len(b) != 2 || b[0] != (osrm.Bearing{Value: 272, Range: 45}) {
		t.Errorf("bearings %+v", b)
	}
}

func TestCreateTripErrors(t *testing.T) {
	hs := newHarness(t, nil)
	ok := map[string]any{"origin": latLon(origin), "destination": latLon(dest)}
	wantError(t, hs.do("POST", "/v1/trips", ok), http.StatusUnauthorized, httpkit.CodeUnauthorized)
	key := []string{"X-API-Key", testKey}
	for name, body := range map[string]any{
		"not JSON":       "{",
		"empty":          "",
		"trailing data":  `{"origin":{"lat":51.1,"lon":71.4}} {}`,
		"no origin":      map[string]any{"destination": latLon(dest)},
		"outside":        map[string]any{"origin": LatLon{Lat: 43.24, Lon: 76.9}, "destination": latLon(dest)}, // Almaty
		"invalid":        map[string]any{"origin": LatLon{Lat: 95, Lon: 71.4}, "destination": latLon(dest)},
		"heading":        map[string]any{"origin": latLon(origin), "destination": latLon(dest), "heading": 360},
		"wrong type":     `{"origin":{"lat":"51.1","lon":71.4},"destination":{"lat":51.15,"lon":71.47}}`,
		"too large body": `{"origin":{"lat":51.1,"lon":71.4},"x":"` + strings.Repeat("a", 5000) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, hs.do("POST", "/v1/trips", body, key...), 400, httpkit.CodeInvalidArgument)
		})
	}

	hs.router.set(osrm.ErrNoRoute)
	wantError(t, hs.do("POST", "/v1/trips", ok, key...), http.StatusNotFound, httpkit.CodeNoRoute)
	hs.router.set(errors.New("connection refused"))
	wantError(t, hs.do("POST", "/v1/trips", ok, key...), http.StatusBadGateway, httpkit.CodeUpstreamUnavailable)
	wantError(t, hs.do("GET", "/v1/trips", nil, key...), http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed)
}

func TestTripTokens(t *testing.T) {
	hs := newHarness(t, nil)
	a, b := hs.createTrip(), hs.createTrip()
	path := "/v1/trips/" + a.Trip.ID

	for _, tok := range []string{a.DriverToken, a.WatchToken} {
		if rec := hs.do("GET", path, nil, bearer(tok)...); rec.Code != http.StatusOK {
			t.Errorf("snapshot with %s…: %d %s", tok[:4], rec.Code, rec.Body.String())
		}
	}
	for name, h := range map[string][]string{
		"no token":           nil,
		"API key":            {"X-API-Key", testKey},
		"other trip's token": bearer(b.DriverToken),
		"other trip watcher": bearer(b.WatchToken),
		"mangled":            bearer(mangle(a.DriverToken)),
		"no prefix":          bearer(a.DriverToken[4:]),
		"wrong scheme":       {"Authorization", "Basic " + a.DriverToken},
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, hs.do("GET", path, nil, h...), http.StatusUnauthorized, httpkit.CodeUnauthorized)
		})
	}
	wantError(t, hs.do("GET", "/v1/trips/0199f0c2-6a1e-7c3b-9d2a-5b1e4c3d2f10", nil, bearer(a.DriverToken)...), http.StatusNotFound, httpkit.CodeNotFound)
	wantError(t, hs.do("GET", "/v1/trips/nope", nil, bearer(a.DriverToken)...), http.StatusNotFound, httpkit.CodeNotFound)

	// The watcher only reads.
	d := &driverSim{hs: hs, trip: a}
	ps := d.along(lineOf(t, a.Route), 0, 20, 10)
	wantError(t, hs.do("POST", path+"/positions", PositionsRequest{Positions: ps}, bearer(a.WatchToken)...), http.StatusForbidden, httpkit.CodeForbidden)
	wantError(t, hs.do("POST", path+"/finish", nil, bearer(a.WatchToken)...), http.StatusForbidden, httpkit.CodeForbidden)
	// Tokens never reach the access log.
	if strings.Contains(hs.logs.String(), a.DriverToken[4:]) || strings.Contains(hs.logs.String(), a.WatchToken[4:]) {
		t.Error("a token appeared in the log")
	}
	if !strings.Contains(hs.logs.String(), `"key_id":"watcher:`) {
		t.Errorf("the access log should name the role:\n%s", hs.logs)
	}
}

func TestDriveAlongTheRoute(t *testing.T) {
	ctx := context.Background()
	hs := newHarness(t, nil)
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)
	d := &driverSim{hs: hs, trip: trip}

	// The first answer carries the route, since the app said it knows none.
	rec := d.send(d.along(line, 0, 30, 10))
	first := decode[PositionsResponse](t, rec)
	if first.Status != "active" || first.RouteVersion != 1 || first.LastSeq != 4 || first.Route == nil || first.Route.Version != 1 {
		t.Fatalf("first answer %+v", first)
	}
	d.known = 1
	drive := d.along(line, 40, 1500, 10) // 147 fixes, 37 batches
	last := d.drive(drive)
	if last.Route != nil || last.Progress == nil || last.Progress.OffRoute {
		t.Fatalf("last answer %+v", last)
	}
	if p := last.Progress; p.Along < 1495 || p.Remaining > line.Length()-1495 || p.RemainingTime <= 0 {
		t.Errorf("progress %+v of %.0f m", p, line.Length())
	}

	// Everything was stored, and one position event per batch written.
	ps, err := hs.store.Positions(ctx, trip.Trip.ID, 0, 1000)
	if err != nil || len(ps) != int(d.seq) {
		t.Fatalf("%d positions stored, sent %d: %v", len(ps), d.seq, err)
	}
	evs, _ := hs.store.EventsAfter(ctx, trip.Trip.ID, 0, 1000)
	if len(evs) != 1+37 {
		t.Errorf("%d events, want one per batch (38)", len(evs))
	}
	var pe PositionEvent
	if len(evs) == 0 || evs[len(evs)-1].Type != EventPosition || json.Unmarshal(evs[len(evs)-1].Data, &pe) != nil {
		t.Fatalf("events %+v", evs)
	}
	if pe.Position.Seq != last.LastSeq || len(pe.Points) == 0 || pe.RouteVersion != 1 || pe.Progress.Along != last.Progress.Along {
		t.Errorf("last event %+v", pe)
	}

	// The watcher's view matches.
	snap := decode[SnapshotResponse](t, hs.do("GET", "/v1/trips/"+trip.Trip.ID, nil, bearer(trip.WatchToken)...))
	if snap.Position == nil || snap.Position.Seq != last.LastSeq || snap.Progress == nil || snap.Progress.Along != last.Progress.Along ||
		snap.LastEventID != evs[len(evs)-1].ID || snap.Route.Version != 1 || snap.Trip.Status != "active" {
		t.Errorf("snapshot %+v", snap)
	}

	// A batch sent again (the app did not get the answer) changes nothing.
	wantError(t, d.send(nil), http.StatusBadRequest, httpkit.CodeInvalidArgument)
	replay := decode[PositionsResponse](t, d.send(drive[len(drive)-3:]))
	if replay.LastSeq != last.LastSeq || replay.Status != "active" {
		t.Errorf("replay answered %+v", replay)
	}
	if again, _ := hs.store.Positions(ctx, trip.Trip.ID, 0, 1000); len(again) != len(ps) {
		t.Errorf("replayed batch stored again: %d positions, had %d", len(again), len(ps))
	}
}

func TestLeavingTheRouteRebuildsIt(t *testing.T) {
	hs := newHarness(t, nil)
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)
	d := &driverSim{hs: hs, trip: trip, known: 1}
	d.drive(d.along(line, 0, 1000, 10))

	// Turn right (south) off the eastbound street.
	turn := line.PointAt(1000)
	var off []PositionIn
	for i := 1; i <= 16; i++ {
		off = append(off, d.fix(geo.Destination(turn, 180, float64(10*i)), 10, 180))
	}
	var got *PositionsResponse
	for i := 0; i < len(off) && got == nil; i += 2 {
		resp := decode[PositionsResponse](t, d.send(off[i:i+2]))
		if resp.Route != nil {
			got = &resp
		}
	}
	if got == nil {
		t.Fatal("no new route after leaving the route")
	}
	if got.RouteVersion != 2 || got.Route.Version != 2 || got.Route.Reason != tracker.ReasonOffRoute || got.Progress.OffRoute ||
		got.Progress.Along != 0 || got.Progress.Remaining != got.Route.Distance {
		t.Errorf("answer with the new route %+v %+v", got, got.Route)
	}
	p := hs.router.last()
	if len(p.Bearings) != 2 || p.Bearings[0].Value != 180 || geo.Distance(p.Points[1], dest) > 0.01 {
		t.Errorf("new route asked with %+v", p)
	}
	d.known = 2

	// Following the new route raises no more new routes.
	nl := lineOf(t, *got.Route)
	if resp := d.drive(d.along(nl, 10, 800, 10)); resp.RouteVersion != 2 || resp.Progress.OffRoute {
		t.Errorf("on the new route: %+v", resp)
	}
	evs, _ := hs.store.EventsAfter(context.Background(), trip.Trip.ID, 0, 1000)
	routes := 0
	for _, e := range evs {
		if e.Type == EventRoute {
			routes++
		}
	}
	if routes != 1 {
		t.Errorf("%d route events, want 1", routes)
	}
	snap := decode[SnapshotResponse](t, hs.do("GET", "/v1/trips/"+trip.Trip.ID, nil, bearer(trip.WatchToken)...))
	if snap.Route.Version != 2 || snap.Progress.Along < 700 {
		t.Errorf("snapshot after the new route %+v %+v", snap.Route, snap.Progress)
	}
}

func TestNewRouteFailureIsRetried(t *testing.T) {
	hs := newHarness(t, nil)
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)
	d := &driverSim{hs: hs, trip: trip, known: 1}
	d.drive(d.along(line, 0, 500, 10))
	hs.router.set(errors.New("osrm restarting"))

	turn := line.PointAt(500)
	var resp PositionsResponse
	for i := 1; i <= 12; i++ {
		resp = decode[PositionsResponse](t, d.send([]PositionIn{d.fix(geo.Destination(turn, 180, float64(10*i)), 10, 180)}))
	}
	if resp.Status != "active" || resp.RouteVersion != 1 || resp.Route != nil || !resp.Progress.OffRoute {
		t.Fatalf("while OSRM is down: %+v", resp)
	}
	if !strings.Contains(hs.logs.String(), "could not build a new route") {
		t.Error("the failure must be logged")
	}

	// OSRM is back; the tracker asks again once its pacing allows.
	hs.router.set(nil)
	var got *RouteDTO
	for i := 13; i <= 40 && got == nil; i++ {
		got = decode[PositionsResponse](t, d.send([]PositionIn{d.fix(geo.Destination(turn, 180, float64(10*i)), 10, 180)})).Route
	}
	if got == nil || got.Version != 2 {
		t.Fatalf("no new route after OSRM came back: %+v", got)
	}
}

func TestArrivalEndsTheTrip(t *testing.T) {
	hs := newHarness(t, nil)
	trip := hs.createTrip()
	line := lineOf(t, trip.Route)
	d := &driverSim{hs: hs, trip: trip, known: 1}
	d.drive(d.along(line, 0, line.Length()-60, 15))
	resp := decode[PositionsResponse](t, d.send([]PositionIn{
		d.fix(line.PointAt(line.Length()-30), 4, 0), d.fix(line.PointAt(line.Length()-20), 1, 0),
	}))
	if resp.Status != "arrived" {
		t.Fatalf("at the destination: %+v", resp)
	}
	wantError(t, d.send([]PositionIn{d.fix(line.End(), 0, 0)}), http.StatusConflict, CodeTripEnded)
	snap := decode[SnapshotResponse](t, hs.do("GET", "/v1/trips/"+trip.Trip.ID, nil, bearer(trip.WatchToken)...))
	if snap.Trip.Status != "arrived" || snap.Trip.EndedAt == nil {
		t.Errorf("snapshot %+v", snap.Trip)
	}
	// Finishing an arrived trip keeps "arrived".
	end := decode[EndResponse](t, hs.do("POST", "/v1/trips/"+trip.Trip.ID+"/finish", nil, bearer(trip.DriverToken)...))
	if end.Status != "arrived" {
		t.Errorf("finish after arrival: %+v", end)
	}
}

func TestFinishAndCancel(t *testing.T) {
	hs := newHarness(t, nil)
	a, b := hs.createTrip(), hs.createTrip()
	hs.clock.add(time.Minute)
	for i := 0; i < 2; i++ { // the second call is a retry
		end := decode[EndResponse](t, hs.do("POST", "/v1/trips/"+a.Trip.ID+"/finish", nil, bearer(a.DriverToken)...))
		if end.Status != "finished" || !end.EndedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("finish #%d: %+v", i+1, end)
		}
	}
	if end := decode[EndResponse](t, hs.do("POST", "/v1/trips/"+a.Trip.ID+"/cancel", nil, bearer(a.DriverToken)...)); end.Status != "finished" {
		t.Errorf("cancel after finish: %+v", end)
	}
	if end := decode[EndResponse](t, hs.do("POST", "/v1/trips/"+b.Trip.ID+"/cancel", nil, bearer(b.DriverToken)...)); end.Status != "cancelled" {
		t.Errorf("cancel: %+v", end)
	}
	evs, _ := hs.store.EventsAfter(context.Background(), a.Trip.ID, 0, 10)
	if len(evs) != 1 || evs[0].Type != store.EventStatus || string(evs[0].Data) != `{"status": "finished"}` {
		t.Errorf("events of the finished trip: %+v", evs)
	}
}

func TestPositionValidation(t *testing.T) {
	hs := newHarness(t, map[string]string{"TRIPS_MAX_BATCH": "5"})
	trip := hs.createTrip()
	d := &driverSim{hs: hs, trip: trip}
	line := lineOf(t, trip.Route)
	path := "/v1/trips/" + trip.Trip.ID + "/positions"
	tok := bearer(trip.DriverToken)

	wantError(t, hs.do("POST", path, PositionsRequest{Positions: d.along(line, 0, 60, 10)}, tok...), 400, httpkit.CodeInvalidArgument)
	for name, body := range map[string]string{
		"no seq":  `{"positions":[{"ts":"2026-10-16T09:00:01Z","lat":51.13,"lon":71.43}]}`,
		"no ts":   `{"positions":[{"seq":1,"lat":51.13,"lon":71.43}]}`,
		"bad ts":  `{"positions":[{"seq":1,"ts":"yesterday","lat":51.13,"lon":71.43}]}`,
		"no lat":  `{"positions":[{"seq":1,"ts":"2026-10-16T09:00:01Z","lon":71.43}]}`,
		"empty":   `{"positions":[]}`,
		"no body": ``,
	} {
		t.Run(name, func(t *testing.T) { wantError(t, hs.do("POST", path, body, tok...), 400, httpkit.CodeInvalidArgument) })
	}

	// A fix from a wrong clock is stored as rejected and does not disturb the rest.
	ps := d.along(line, 0, 30, 10)
	future := ps[1].TS.Add(time.Hour)
	ps[1].TS = future
	resp := decode[PositionsResponse](t, hs.do("POST", path, PositionsRequest{Positions: ps}, tok...))
	if resp.LastSeq != d.seq || resp.Status != "active" {
		t.Errorf("answer %+v", resp)
	}
	stored, _ := hs.store.Positions(context.Background(), trip.Trip.ID, 0, 10)
	if len(stored) != 4 || stored[1].Reject != rejectBadTime || stored[0].Reject != "" || stored[2].Class != tracker.ClassOn {
		t.Errorf("stored %+v", stored)
	}
}

func TestPerTripRateLimit(t *testing.T) {
	hs := newHarness(t, map[string]string{"TRIPS_RATE_LIMIT_TRIP_RPS": "1", "TRIPS_RATE_LIMIT_TRIP_BURST": "1"})
	a, b := hs.createTrip(), hs.createTrip()
	if rec := hs.do("GET", "/v1/trips/"+a.Trip.ID, nil, bearer(a.WatchToken)...); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	rec := hs.do("GET", "/v1/trips/"+a.Trip.ID, nil, bearer(a.WatchToken)...)
	wantError(t, rec, http.StatusTooManyRequests, httpkit.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Other trips, and the driver of the same trip, have buckets of their own.
	if rec := hs.do("GET", "/v1/trips/"+a.Trip.ID, nil, bearer(a.DriverToken)...); rec.Code != 200 {
		t.Errorf("driver of the same trip: %d", rec.Code)
	}
	if rec := hs.do("GET", "/v1/trips/"+b.Trip.ID, nil, bearer(b.WatchToken)...); rec.Code != 200 {
		t.Errorf("another trip: %d", rec.Code)
	}
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	hs := newHarness(t, map[string]string{"TRIPS_IDLE_TIMEOUT": "30m", "TRIPS_RETENTION": "48h"})
	idle, busy := hs.createTrip(), hs.createTrip()
	d := &driverSim{hs: hs, trip: busy, known: 1}
	line := lineOf(t, busy.Route)
	hs.clock.add(25 * time.Minute)
	d.drive(d.along(line, 0, 40, 10))
	hs.clock.add(10 * time.Minute)

	hs.srv.Sweep(ctx)
	if tr, _ := hs.store.Trip(ctx, idle.Trip.ID); tr.Status != store.Expired {
		t.Errorf("idle trip %s", tr.Status)
	}
	if tr, _ := hs.store.Trip(ctx, busy.Trip.ID); tr.Status != store.Active {
		t.Errorf("busy trip %s", tr.Status)
	}
	silent := &driverSim{hs: hs, trip: idle, known: 1}
	wantError(t, silent.send(silent.along(line, 0, 10, 10)), http.StatusConflict, CodeTripEnded)

	hs.clock.add(49 * time.Hour)
	hs.srv.Sweep(ctx) // busy expires now, idle is deleted
	if _, err := hs.store.Trip(ctx, idle.Trip.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old trip not deleted: %v", err)
	}
	if tr, _ := hs.store.Trip(ctx, busy.Trip.ID); tr.Status != store.Expired {
		t.Errorf("busy trip after two days %s", tr.Status)
	}
}

func TestHealth(t *testing.T) {
	hs := newHarness(t, nil)
	if rec := hs.do("GET", "/healthz", nil); rec.Code != 200 {
		t.Errorf("healthz %d", rec.Code)
	}
	rep := decode[Readiness](t, hs.do("GET", "/readyz", nil))
	if rep.Status != "ok" || rep.Checks["db"] != "ok" || rep.Checks["osrm-car"] != "ok" {
		t.Errorf("ready %+v", rep)
	}
	hs.router.healthErr = errors.New("down")
	rec := hs.do("GET", "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable || decode[Readiness](t, rec).Checks["osrm-car"] != "down" {
		t.Errorf("readyz with OSRM down: %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, hs.do("GET", "/v1/nope", nil), http.StatusNotFound, httpkit.CodeNotFound)
}

func TestParseBearer(t *testing.T) {
	tok, _, err := newToken(driver)
	if err != nil {
		t.Fatal(err)
	}
	if got, r, ok := parseBearer("Bearer " + tok); !ok || got != tok || r != driver {
		t.Errorf("parse %q: %q %q %v", tok, got, r, ok)
	}
	if _, r, ok := parseBearer("bearer  wch_" + tok[4:]); !ok || r != watcher {
		t.Errorf("watcher token, lower-case scheme: %q %v", r, ok)
	}
	for _, h := range []string{"", "Bearer", "Bearer ", "Token " + tok, "Bearer " + tok + "x", "Bearer xyz_" + tok[4:], "Bearer drv_" + strings.Repeat("!", 43)} {
		if _, _, ok := parseBearer(h); ok {
			t.Errorf("%q accepted", h)
		}
	}
}

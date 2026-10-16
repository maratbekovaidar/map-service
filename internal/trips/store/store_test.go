package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"map-service/internal/geo"
	"map-service/internal/trips/tracker"
)

// The database tests need a PostgreSQL to work in: `make test-trips-db`
// starts a throwaway one and sets TRIPS_TEST_DATABASE_URL. Each test gets a
// schema of its own.
const dsnEnv = "TRIPS_TEST_DATABASE_URL"

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// clock is a settable store clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newClock makes s run on a clock that starts at t0.
func newClock(s *Store) *clock { c := &clock{t: t0}; s.SetClock(c.now); return c }

// freshStore returns a store on an empty schema, not migrated yet.
func freshStore(t *testing.T) *Store {
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
	schema := "test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	s, err := open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s := freshStore(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

var (
	origin = geo.Point{Lon: 71.4306, Lat: 51.1283}
	dest   = geo.Point{Lon: 71.47, Lat: 51.18}
)

func token(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// route builds a 3-point route geometry from `from`, eastwards.
func route(from geo.Point) Route {
	pts := []geo.Point{from, geo.Destination(from, 90, 600), geo.Destination(from, 90, 1000)}
	return Route{
		From: from, Heading: -1, Distance: 1000, Duration: 90,
		Geometry: geo.EncodePolyline(pts, 6), SegDurations: []float64{50, 30}, DataVersion: "20261014T072150Z",
	}
}

func newTrip(t *testing.T, s *Store) *Trip {
	t.Helper()
	trip, err := s.CreateTrip(context.Background(), NewTrip{
		Profile: "car", Origin: origin, Destination: dest,
		DriverTokenHash: token(t), WatchTokenHash: token(t), Route: route(origin),
	})
	if err != nil {
		t.Fatal(err)
	}
	return trip
}

func TestMigrate(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	done, err := s.Migrate(ctx)
	if err != nil || !slices.Equal(done, []string{"0001_init"}) {
		t.Fatalf("first run applied %v, %v", done, err)
	}
	if done, err := s.Migrate(ctx); err != nil || len(done) != 0 {
		t.Errorf("second run applied %v, %v", done, err)
	}

	// Several instances starting at once: one applies, the others wait.
	s2 := freshStore(t)
	var wg sync.WaitGroup
	results := make([][]string, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = s2.Migrate(ctx) })
	}
	wg.Wait()
	applied := 0
	for i := range results {
		if errs[i] != nil {
			t.Errorf("concurrent Migrate: %v", errs[i])
		}
		applied += len(results[i])
	}
	if applied != 1 {
		t.Errorf("concurrent Migrate applied %d migrations in all, want 1", applied)
	}
}

func TestMigrationNames(t *testing.T) {
	all, err := migrations()
	if err != nil || len(all) == 0 || all[0].version != 1 {
		t.Fatalf("migrations: %v %v", all, err)
	}
}

func TestCreateTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	newClock(s)
	trip := newTrip(t, s)
	if !validID(trip.ID) || trip.Status != Active || trip.RouteVersion != 1 || !trip.CreatedAt.Equal(t0) ||
		!trip.LastSeenAt.Equal(t0) || !trip.EndedAt.IsZero() || trip.Origin != origin || trip.Destination != dest {
		t.Errorf("created %+v", trip)
	}

	got, err := s.Trip(ctx, trip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != trip.ID || string(got.DriverTokenHash) != string(trip.DriverTokenHash) || got.Profile != "car" {
		t.Errorf("read back %+v", got)
	}
	routes, err := s.Routes(ctx, trip.ID)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes %v %v", routes, err)
	}
	r := routes[0]
	if r.Version != 1 || r.Reason != ReasonInitial || r.Heading != -1 || !slices.Equal(r.SegDurations, []float64{50, 30}) ||
		r.Geometry != route(origin).Geometry || r.DataVersion != "20261014T072150Z" {
		t.Errorf("route %+v", r)
	}
	tr, err := r.Tracker()
	if err != nil || tr.Version != 1 || tr.Line.Segments() != 2 {
		t.Fatalf("tracker route %+v %v", tr, err)
	}
	if _, secs := tr.Remaining(0); secs < 89.99 || secs > 90.01 {
		t.Errorf("remaining time at the start %.2f, want 90", secs)
	}

	for _, id := range []string{"nope", "00000000-0000-0000-0000-000000000000", "0198f3a4-zzzz-7000-8000-000000000000"} {
		if _, err := s.Trip(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Trip(%q): %v", id, err)
		}
	}
	// Token hashes are unique.
	_, err = s.CreateTrip(ctx, NewTrip{Profile: "car", DriverTokenHash: trip.DriverTokenHash, WatchTokenHash: token(t), Route: route(origin)})
	if err == nil {
		t.Error("a reused token hash must be rejected")
	}
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	clk := newClock(s)
	trip := newTrip(t, s)
	clk.add(time.Minute)

	fixes := []tracker.Fix{
		{Seq: 1, Time: t0.Add(55 * time.Second), Pos: origin, Accuracy: 6, Speed: 11.5, Heading: 87},
		{Seq: 2, Time: t0.Add(56 * time.Second), Pos: geo.Destination(origin, 90, 12), Accuracy: 0, Speed: -1, Heading: -1},
		{Seq: 3, Time: t0.Add(57 * time.Second), Pos: geo.Destination(origin, 90, 24), Accuracy: 90, Speed: 12, Heading: 90},
	}
	results := []tracker.FixResult{
		{Seq: 1, Class: tracker.ClassOn, Along: 0, Offset: 0.4},
		{Seq: 2, Class: tracker.ClassNear, Along: 12, Offset: 17.5},
		{Seq: 3, Reject: tracker.RejectInaccurate},
	}
	var ids []int64
	err := s.Update(ctx, trip.ID, func(ctx context.Context, tx *Tx) error {
		if !tx.Now().Equal(t0.Add(time.Minute)) {
			t.Errorf("tx time %v", tx.Now())
		}
		var ps []Position
		for i := range fixes {
			ps = append(ps, PositionOf(fixes[i], results[i], tx.Trip.RouteVersion, tx.Now()))
		}
		if n, err := tx.AddPositions(ctx, ps); err != nil || n != 3 {
			return errors.Join(err, errors.New("want 3 new positions"))
		}
		v, err := tx.AddRoute(ctx, Route{Reason: tracker.ReasonOffRoute, From: fixes[1].Pos, Heading: 91.5,
			Distance: 2000, Duration: 150, Geometry: route(fixes[1].Pos).Geometry, DataVersion: "v"})
		if err != nil || v != 2 || tx.Trip.RouteVersion != 2 {
			return errors.Join(err, errors.New("want route version 2"))
		}
		for _, typ := range []string{"position", "route"} {
			id, err := tx.AddEvent(ctx, typ, map[string]any{"seq": 2})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		tx.Trip.State.Along, tx.Trip.State.LastSeq = 12, 3
		tx.Trip.LastSeenAt = tx.Now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Trip(ctx, trip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RouteVersion != 2 || got.State.Along != 12 || got.State.LastSeq != 3 ||
		!got.LastSeenAt.Equal(t0.Add(time.Minute)) || !got.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("trip after update %+v", got)
	}

	ps, err := s.Positions(ctx, trip.ID, 0, 10)
	if err != nil || len(ps) != 3 {
		t.Fatalf("positions %v %v", ps, err)
	}
	if p := ps[0]; p.Seq != 1 || !p.DeviceTime.Equal(fixes[0].Time) || p.Pos != origin || p.Accuracy != 6 ||
		p.Speed != 11.5 || p.Heading != 87 || p.Class != tracker.ClassOn || p.Offset != 0.4 || p.RouteVersion != 1 {
		t.Errorf("position 1 %+v", p)
	}
	if p := ps[1]; p.Accuracy != 0 || p.Speed != -1 || p.Heading != -1 || p.Class != tracker.ClassNear || p.Along != 12 {
		t.Errorf("unreported values must come back as unknown: %+v", p)
	}
	if p := ps[2]; p.Reject != tracker.RejectInaccurate || p.Class != "" || p.Along != 0 || p.Accuracy != 90 {
		t.Errorf("rejected position %+v", p)
	}
	if ps, _ := s.Positions(ctx, trip.ID, 2, 10); len(ps) != 1 || ps[0].Seq != 3 {
		t.Errorf("positions after seq 2: %+v", ps)
	}

	routes, _ := s.Routes(ctx, trip.ID)
	if len(routes) != 2 || routes[1].Reason != tracker.ReasonOffRoute || routes[1].Heading != 91.5 || routes[1].SegDurations != nil {
		t.Errorf("routes %+v", routes)
	}

	evs, err := s.EventsAfter(ctx, trip.ID, 0, 10)
	if err != nil || len(evs) != 2 || evs[0].ID != ids[0] || evs[1].ID != ids[1] || ids[0] >= ids[1] ||
		evs[1].Type != "route" || string(evs[0].Data) != `{"seq": 2}` || evs[0].TripID != trip.ID {
		t.Errorf("events %+v %v", evs, err)
	}
	if evs, _ := s.EventsAfter(ctx, trip.ID, ids[0], 10); len(evs) != 1 || evs[0].ID != ids[1] {
		t.Errorf("events after the first: %+v", evs)
	}

	// A batch sent again stores nothing new; a failing fn leaves no trace.
	boom := errors.New("boom")
	err = s.Update(ctx, trip.ID, func(ctx context.Context, tx *Tx) error {
		n, err := tx.AddPositions(ctx, []Position{PositionOf(fixes[0], results[0], 1, tx.Now())})
		if err != nil || n != 0 {
			t.Errorf("replayed position: %d new, %v", n, err)
		}
		if _, err := tx.AddEvent(ctx, "position", 1); err != nil {
			return err
		}
		tx.Trip.State.Along = 999
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v", err)
	}
	if got, _ := s.Trip(ctx, trip.ID); got.State.Along != 12 {
		t.Errorf("rolled back state written: along %v", got.State.Along)
	}
	if evs, _ := s.EventsAfter(ctx, trip.ID, 0, 10); len(evs) != 2 {
		t.Errorf("rolled back event written: %d events", len(evs))
	}
	if err := s.Update(ctx, "00000000-0000-0000-0000-000000000000", func(context.Context, *Tx) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update of a missing trip: %v", err)
	}
}

func TestUpdateSerialisesATrip(t *testing.T) {
	// Read-modify-write of the state from several goroutines loses nothing.
	ctx := context.Background()
	s := testStore(t)
	trip := newTrip(t, s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := s.Update(ctx, trip.ID, func(ctx context.Context, tx *Tx) error {
				n := tx.Trip.State.Reroutes
				time.Sleep(5 * time.Millisecond)
				tx.Trip.State.Reroutes = n + 1
				_, err := tx.AddEvent(ctx, "tick", n)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, _ := s.Trip(ctx, trip.ID)
	if got.State.Reroutes != 8 {
		t.Errorf("counter %d, want 8", got.State.Reroutes)
	}
	// Events are numbered in the order the updates ran.
	evs, _ := s.EventsAfter(ctx, trip.ID, 0, 100)
	for i, e := range evs {
		var n int
		if err := json.Unmarshal(e.Data, &n); err != nil || n != i {
			t.Errorf("event %d carries %s", i, e.Data)
		}
	}
}

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	trip := newTrip(t, s)

	snap, err := s.Snapshot(ctx, trip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Trip.ID != trip.ID || snap.Route.Version != 1 || snap.Position != nil || snap.LastEventID != 0 {
		t.Errorf("empty snapshot %+v", snap)
	}

	var last int64
	err = s.Update(ctx, trip.ID, func(ctx context.Context, tx *Tx) error {
		accepted := tracker.Fix{Seq: 1, Time: t0, Pos: origin, Speed: -1, Heading: -1}
		rejected := tracker.Fix{Seq: 2, Time: t0.Add(time.Second), Pos: dest, Speed: -1, Heading: -1}
		if _, err := tx.AddPositions(ctx, []Position{
			PositionOf(accepted, tracker.FixResult{Class: tracker.ClassOn}, 1, t0),
			PositionOf(rejected, tracker.FixResult{Reject: tracker.RejectJump}, 1, t0),
		}); err != nil {
			return err
		}
		r := route(origin)
		r.Reason = tracker.ReasonOffRoute
		if _, err := tx.AddRoute(ctx, r); err != nil {
			return err
		}
		var err error
		last, err = tx.AddEvent(ctx, "route", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err = s.Snapshot(ctx, trip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Route.Version != 2 || snap.Position == nil || snap.Position.Seq != 1 || snap.LastEventID != last {
		t.Errorf("snapshot %+v, position %+v", snap, snap.Position)
	}
	if _, err := s.Snapshot(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("snapshot of a missing trip: %v", err)
	}
}

func TestEndExpirePurge(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	clk := newClock(s)

	finished, idle, busy := newTrip(t, s), newTrip(t, s), newTrip(t, s)
	clk.add(20 * time.Minute)
	err := s.Update(ctx, finished.ID, func(ctx context.Context, tx *Tx) error {
		if err := tx.End(Active); err == nil {
			t.Error("Active must not end a trip")
		}
		if err := tx.End(Finished); err != nil {
			return err
		}
		if err := tx.End(Cancelled); err == nil {
			t.Error("an ended trip must not end again")
		}
		_, err := tx.AddEvent(ctx, EventStatus, StatusEvent{Status: Finished})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Trip(ctx, finished.ID)
	if got.Status != Finished || !got.EndedAt.Equal(clk.now()) {
		t.Errorf("finished trip %+v", got)
	}

	// busy sends positions; idle has been silent for 30 minutes.
	clk.add(10 * time.Minute)
	if err := s.Update(ctx, busy.ID, func(_ context.Context, tx *Tx) error { tx.Trip.LastSeenAt = tx.Now(); return nil }); err != nil {
		t.Fatal(err)
	}
	expired, err := s.ExpireIdle(ctx, clk.now().Add(-25*time.Minute))
	if err != nil || !slices.Equal(expired, []string{idle.ID}) {
		t.Fatalf("expired %v %v", expired, err)
	}
	got, _ = s.Trip(ctx, idle.ID)
	if got.Status != Expired || !got.EndedAt.Equal(clk.now()) {
		t.Errorf("idle trip %+v", got)
	}
	evs, _ := s.EventsAfter(ctx, idle.ID, 0, 10)
	var se StatusEvent
	if len(evs) != 1 || evs[0].Type != EventStatus || json.Unmarshal(evs[0].Data, &se) != nil || se.Status != Expired {
		t.Errorf("expiry event %+v", evs)
	}
	if again, _ := s.ExpireIdle(ctx, clk.now().Add(-25*time.Minute)); len(again) != 0 {
		t.Errorf("expired twice: %v", again)
	}

	// Purging what ended before t0+25m removes the finished trip with all its
	// rows and keeps the trip that expired later and the active one.
	err = s.Update(ctx, busy.ID, func(ctx context.Context, tx *Tx) error {
		_, err := tx.AddPositions(ctx, []Position{{Seq: 1, DeviceTime: t0, ReceivedAt: t0, Pos: origin, Speed: -1, Heading: -1}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Purge(ctx, finished.CreatedAt.Add(25*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("purged %d %v", n, err)
	}
	if _, err := s.Trip(ctx, finished.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("purged trip still there: %v", err)
	}
	if evs, _ := s.EventsAfter(ctx, finished.ID, 0, 10); len(evs) != 0 {
		t.Errorf("events of a purged trip: %v", evs)
	}
	for _, id := range []string{idle.ID, busy.ID} {
		if _, err := s.Trip(ctx, id); err != nil {
			t.Errorf("trip %s purged too early: %v", id, err)
		}
	}
	if ps, _ := s.Positions(ctx, busy.ID, 0, 10); len(ps) != 1 {
		t.Errorf("active trip lost its positions: %v", ps)
	}
}

func TestListen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := testStore(t)
	trip := newTrip(t, s)

	notes := make(chan Notification, 16)
	done := make(chan error, 1)
	go func() { done <- s.Listen(ctx, func(n Notification) { notes <- n }) }()
	next := func() Notification {
		t.Helper()
		select {
		case n := <-notes:
			return n
		case <-time.After(5 * time.Second):
			t.Fatal("no notification")
		}
		return Notification{}
	}
	if n := next(); n != (Notification{}) {
		t.Fatalf("first notification %+v, want the zero one", n)
	}

	var id int64
	err := s.Update(context.Background(), trip.ID, func(ctx context.Context, tx *Tx) error {
		var err error
		id, err = tx.AddEvent(ctx, "position", 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := next(); n.TripID != trip.ID || n.EventID != id {
		t.Errorf("notification %+v, want %s:%d", n, trip.ID, id)
	}

	expiring := newTrip(t, s)
	if _, err := s.ExpireIdle(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 { // both trips expire
		seen[next().TripID] = true
	}
	if !seen[trip.ID] || !seen[expiring.ID] {
		t.Errorf("expiry notifications for %v", seen)
	}

	// The listening connection dies: Listen reconnects, says that it may have
	// missed notifications, and goes on delivering.
	tag, err := s.pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE query = 'LISTEN `+notifyChannel+`' AND pid <> pg_backend_pid()`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("terminated %d listeners: %v", tag.RowsAffected(), err)
	}
	if n := next(); n != (Notification{}) {
		t.Fatalf("after reconnecting: %+v, want the zero notification", n)
	}
	if err := s.Update(context.Background(), trip.ID, func(ctx context.Context, tx *Tx) error {
		id, err = tx.AddEvent(ctx, "position", 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := next(); n.EventID != id {
		t.Errorf("after reconnecting: %+v, want event %d", n, id)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Listen returned %v", err)
	}
}

func TestParseNotification(t *testing.T) {
	id := "0199f0c2-6a1e-7c3b-9d2a-5b1e4c3d2f10"
	if n, ok := parseNotification(id + ":42"); !ok || n.TripID != id || n.EventID != 42 {
		t.Errorf("parsed %+v %v", n, ok)
	}
	for _, p := range []string{"", id, id + ":", id + ":x", id + ":0", "nope:1", ":1"} {
		if _, ok := parseNotification(p); ok {
			t.Errorf("%q parsed", p)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"0199f0c2-6a1e-7c3b-9d2a-5b1e4c3d2f10": true,
		"0199F0C2-6A1E-7C3B-9D2A-5B1E4C3D2F10": true,
		"0199f0c26a1e7c3b9d2a5b1e4c3d2f10":     false,
		"0199f0c2-6a1e-7c3b-9d2a-5b1e4c3d2f1g": false,
		"0199f0c2+6a1e-7c3b-9d2a-5b1e4c3d2f10": false,
		"":                                     false,
	} {
		if validID(id) != want {
			t.Errorf("validID(%q) = %v", id, !want)
		}
	}
}

// Package store keeps trips in PostgreSQL (the trips-db service): trips
// with their tracker state, every route a trip had, every position the
// driver sent and the event log watchers follow.
//
// Writes that belong to one trip go through Update, which holds the trip's
// row lock for the whole transaction. That serialises the batches of one
// trip and keeps its events in commit order, which Snapshot and EventsAfter
// rely on.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"map-service/internal/geo"
	"map-service/internal/trips/tracker"
)

// ErrNotFound means there is no trip with that id (or no such route).
var ErrNotFound = errors.New("store: not found")

// Status is the lifecycle state of a trip.
type Status string

// Trip statuses. Every status but Active ends the trip.
const (
	Active    Status = "active"
	Arrived   Status = "arrived"   // the tracker saw the car arrive
	Finished  Status = "finished"  // the driver ended the trip
	Cancelled Status = "cancelled" // the driver cancelled it
	Expired   Status = "expired"   // no positions for too long
)

// Route reasons besides tracker.ReasonOffRoute and tracker.ReasonWrongWay.
const ReasonInitial = "initial"

// EventStatus is the type of the event written when a trip ends; its data is
// a StatusEvent.
const EventStatus = "status"

// StatusEvent is the data of an EventStatus event.
type StatusEvent struct {
	Status Status `json:"status"`
}

// notifyChannel is the PostgreSQL NOTIFY channel for new events; the payload
// is "<trip id>:<event id>".
const notifyChannel = "trip_events"

// Trip is one trip.
type Trip struct {
	ID              string
	Status          Status
	Profile         string
	Origin          geo.Point
	Destination     geo.Point
	DriverTokenHash []byte
	WatchTokenHash  []byte
	RouteVersion    int // the current route
	State           tracker.State
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastSeenAt      time.Time // last batch of positions
	EndedAt         time.Time // zero while active
}

// Route is one version of a trip's route.
type Route struct {
	Version      int
	Reason       string    // ReasonInitial, tracker.ReasonOffRoute or tracker.ReasonWrongWay
	From         geo.Point // where it was built from
	Heading      float64   // direction of travel it was built for; negative if unknown
	Distance     float64   // metres
	Duration     float64   // seconds
	Geometry     string    // encoded polyline, precision 6
	SegDurations []float64 // seconds per geometry segment; nil if unknown
	DataVersion  string    // map data version
	CreatedAt    time.Time
}

// Tracker turns the route into the tracker's form.
func (r *Route) Tracker() (*tracker.Route, error) {
	pts, err := geo.DecodePolyline(r.Geometry, 6)
	if err != nil {
		return nil, fmt.Errorf("route %d: %w", r.Version, err)
	}
	return tracker.NewRoute(r.Version, pts, r.Duration, r.SegDurations)
}

// Position is one position the driver sent and what the tracker made of it.
type Position struct {
	Seq          int64
	DeviceTime   time.Time
	ReceivedAt   time.Time
	Pos          geo.Point
	Accuracy     float64 // metres; 0 if not reported
	Speed        float64 // m/s; negative if not reported
	Heading      float64 // degrees; negative if not reported
	RouteVersion int     // the route it was judged against
	Reject       string  // why the tracker ignored it; empty if accepted
	Class        tracker.Class
	Along        float64 // metres along the route (accepted positions)
	Offset       float64 // metres from the route (accepted positions)
}

// PositionOf records fix f with the tracker's result for it.
func PositionOf(f tracker.Fix, res tracker.FixResult, routeVersion int, received time.Time) Position {
	return Position{
		Seq: f.Seq, DeviceTime: f.Time, ReceivedAt: received, Pos: f.Pos,
		Accuracy: f.Accuracy, Speed: f.Speed, Heading: f.Heading, RouteVersion: routeVersion,
		Reject: res.Reject, Class: res.Class, Along: res.Along, Offset: res.Offset,
	}
}

// Event is one entry of a trip's event log.
type Event struct {
	ID        int64
	TripID    string
	Type      string
	Data      json.RawMessage
	CreatedAt time.Time
}

// Store is the trips database.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Open connects to the database at dsn (a postgres:// URL or key=value
// string) and checks that it answers.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return open(ctx, cfg)
}

func open(ctx context.Context, cfg *pgxpool.Config) (*Store, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{pool: pool, now: time.Now}, nil
}

// SetClock replaces the clock that stamps created, updated and ended times
// (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes all connections.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the connection (readiness probes).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// validID reports whether id looks like a UUID, so that malformed ids from
// URLs are "not found" instead of a database error.
func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

const tripColumns = `id, status, profile, origin_lon, origin_lat, dest_lon, dest_lat,
	driver_token_hash, watch_token_hash, route_version, tracker_state,
	created_at, updated_at, last_seen_at, ended_at`

func scanTrip(row pgx.Row) (*Trip, error) {
	var (
		t     Trip
		state []byte
		ended *time.Time
	)
	err := row.Scan(&t.ID, &t.Status, &t.Profile, &t.Origin.Lon, &t.Origin.Lat, &t.Destination.Lon, &t.Destination.Lat,
		&t.DriverTokenHash, &t.WatchTokenHash, &t.RouteVersion, &state,
		&t.CreatedAt, &t.UpdatedAt, &t.LastSeenAt, &ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(state, &t.State); err != nil {
		return nil, fmt.Errorf("store: trip %s: tracker state: %w", t.ID, err)
	}
	if ended != nil {
		t.EndedAt = *ended
	}
	return &t, nil
}

// NewTrip is what CreateTrip needs.
type NewTrip struct {
	Profile         string
	Origin          geo.Point
	Destination     geo.Point
	DriverTokenHash []byte
	WatchTokenHash  []byte
	// Route is the first route; CreateTrip sets its Version and Reason.
	Route Route
}

// CreateTrip stores a new active trip with its first route.
func (s *Store) CreateTrip(ctx context.Context, nt NewTrip) (*Trip, error) {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	trip, err := scanTrip(tx.QueryRow(ctx, `
		INSERT INTO trips (profile, origin_lon, origin_lat, dest_lon, dest_lat,
			driver_token_hash, watch_token_hash, route_version, created_at, updated_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, $8, $8)
		RETURNING `+tripColumns,
		nt.Profile, nt.Origin.Lon, nt.Origin.Lat, nt.Destination.Lon, nt.Destination.Lat,
		nt.DriverTokenHash, nt.WatchTokenHash, now))
	if err != nil {
		return nil, fmt.Errorf("store: create trip: %w", err)
	}
	r := nt.Route
	r.Version, r.Reason = 1, ReasonInitial
	if err := insertRoute(ctx, tx, trip.ID, r, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return trip, nil
}

func insertRoute(ctx context.Context, q pgx.Tx, tripID string, r Route, now time.Time) error {
	var heading *float64
	if r.Heading >= 0 {
		heading = &r.Heading
	}
	_, err := q.Exec(ctx, `
		INSERT INTO trip_routes (trip_id, version, reason, from_lon, from_lat, heading,
			distance_m, duration_s, geometry, seg_durations, data_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		tripID, r.Version, r.Reason, r.From.Lon, r.From.Lat, heading,
		r.Distance, r.Duration, r.Geometry, r.SegDurations, r.DataVersion, now)
	if err != nil {
		return fmt.Errorf("store: route %d of trip %s: %w", r.Version, tripID, err)
	}
	return nil
}

// Trip returns one trip.
func (s *Store) Trip(ctx context.Context, id string) (*Trip, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	return scanTrip(s.pool.QueryRow(ctx, `SELECT `+tripColumns+` FROM trips WHERE id = $1`, id))
}

const routeColumns = `version, reason, from_lon, from_lat, heading, distance_m, duration_s,
	geometry, seg_durations, data_version, created_at`

func scanRoute(row pgx.Row) (*Route, error) {
	var (
		r       Route
		heading *float64
	)
	err := row.Scan(&r.Version, &r.Reason, &r.From.Lon, &r.From.Lat, &heading, &r.Distance, &r.Duration,
		&r.Geometry, &r.SegDurations, &r.DataVersion, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Heading = -1
	if heading != nil {
		r.Heading = *heading
	}
	return &r, nil
}

// Routes returns every route of a trip, oldest first.
func (s *Store) Routes(ctx context.Context, tripID string) ([]Route, error) {
	if !validID(tripID) {
		return nil, ErrNotFound
	}
	rows, _ := s.pool.Query(ctx, `SELECT `+routeColumns+` FROM trip_routes WHERE trip_id = $1 ORDER BY version`, tripID)
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Route, error) {
		r, err := scanRoute(row)
		if err != nil {
			return Route{}, err
		}
		return *r, nil
	})
}

const positionColumns = `seq, device_time, received_at, lon, lat, accuracy_m, speed_mps, heading,
	route_version, reject, class, along_m, offset_m`

func scanPosition(row pgx.Row) (Position, error) {
	var (
		p                     Position
		accuracy, speed, head *float32
		reject, class         *string
		along, offset         *float64
	)
	err := row.Scan(&p.Seq, &p.DeviceTime, &p.ReceivedAt, &p.Pos.Lon, &p.Pos.Lat, &accuracy, &speed, &head,
		&p.RouteVersion, &reject, &class, &along, &offset)
	if err != nil {
		return p, err
	}
	p.Speed, p.Heading = -1, -1
	if accuracy != nil {
		p.Accuracy = float64(*accuracy)
	}
	if speed != nil {
		p.Speed = float64(*speed)
	}
	if head != nil {
		p.Heading = float64(*head)
	}
	if reject != nil {
		p.Reject = *reject
	}
	if class != nil {
		p.Class = tracker.Class(*class)
	}
	if along != nil {
		p.Along = *along
	}
	if offset != nil {
		p.Offset = *offset
	}
	return p, nil
}

// Positions returns up to limit positions of a trip with seq > afterSeq, in
// seq order.
func (s *Store) Positions(ctx context.Context, tripID string, afterSeq int64, limit int) ([]Position, error) {
	if !validID(tripID) {
		return nil, ErrNotFound
	}
	rows, _ := s.pool.Query(ctx, `SELECT `+positionColumns+` FROM trip_positions
		WHERE trip_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, tripID, afterSeq, limit)
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Position, error) { return scanPosition(row) })
}

// EventsAfter returns up to limit events of a trip with id > after, in order.
func (s *Store) EventsAfter(ctx context.Context, tripID string, after int64, limit int) ([]Event, error) {
	if !validID(tripID) {
		return nil, ErrNotFound
	}
	rows, _ := s.pool.Query(ctx, `SELECT id, trip_id, type, data, created_at FROM trip_events
		WHERE trip_id = $1 AND id > $2 ORDER BY id LIMIT $3`, tripID, after, limit)
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.TripID, &e.Type, &e.Data, &e.CreatedAt)
		return e, err
	})
}

// Snapshot is a consistent view of a trip for a new watcher.
type Snapshot struct {
	Trip        *Trip
	Route       *Route    // the current route
	Position    *Position // the last accepted position; nil before the first
	LastEventID int64     // follow events after this one; 0 if there are none
}

// Snapshot reads a trip, its current route, its last accepted position and
// its latest event id in one consistent view: a watcher that shows it and
// then follows EventsAfter(LastEventID) misses nothing and sees nothing twice.
func (s *Store) Snapshot(ctx context.Context, tripID string) (*Snapshot, error) {
	if !validID(tripID) {
		return nil, ErrNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	snap := &Snapshot{}
	if snap.Trip, err = scanTrip(tx.QueryRow(ctx, `SELECT `+tripColumns+` FROM trips WHERE id = $1`, tripID)); err != nil {
		return nil, err
	}
	if snap.Route, err = scanRoute(tx.QueryRow(ctx, `SELECT `+routeColumns+` FROM trip_routes
		WHERE trip_id = $1 AND version = $2`, tripID, snap.Trip.RouteVersion)); err != nil {
		return nil, fmt.Errorf("store: current route of trip %s: %w", tripID, err)
	}
	p, err := scanPosition(tx.QueryRow(ctx, `SELECT `+positionColumns+` FROM trip_positions
		WHERE trip_id = $1 AND reject IS NULL ORDER BY seq DESC LIMIT 1`, tripID))
	switch {
	case err == nil:
		snap.Position = &p
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM trip_events WHERE trip_id = $1`, tripID).
		Scan(&snap.LastEventID); err != nil {
		return nil, err
	}
	return snap, tx.Commit(ctx)
}

// Tx is a transaction holding the row lock of one trip.
type Tx struct {
	tx  pgx.Tx
	now time.Time
	// Trip is the locked trip. Changes to its Status, RouteVersion, State,
	// LastSeenAt and EndedAt are written back when Update commits.
	Trip *Trip
}

// Now is the store clock at the start of the transaction.
func (tx *Tx) Now() time.Time { return tx.now }

// Update locks the trip, runs fn and commits, writing back tx.Trip; if fn
// returns an error everything is rolled back and Update returns that error.
func (s *Store) Update(ctx context.Context, tripID string, fn func(ctx context.Context, tx *Tx) error) error {
	if !validID(tripID) {
		return ErrNotFound
	}
	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = pgtx.Rollback(context.WithoutCancel(ctx)) }()
	trip, err := scanTrip(pgtx.QueryRow(ctx, `SELECT `+tripColumns+` FROM trips WHERE id = $1 FOR UPDATE`, tripID))
	if err != nil {
		return err
	}
	tx := &Tx{tx: pgtx, now: s.now(), Trip: trip}
	if err := fn(ctx, tx); err != nil {
		return err
	}

	state, err := json.Marshal(trip.State)
	if err != nil {
		return fmt.Errorf("store: tracker state: %w", err)
	}
	var ended *time.Time
	if !trip.EndedAt.IsZero() {
		ended = &trip.EndedAt
	}
	if _, err := pgtx.Exec(ctx, `UPDATE trips SET status = $2, route_version = $3, tracker_state = $4,
		last_seen_at = $5, ended_at = $6, updated_at = $7 WHERE id = $1`,
		trip.ID, trip.Status, trip.RouteVersion, state, trip.LastSeenAt, ended, tx.now); err != nil {
		return fmt.Errorf("store: update trip %s: %w", trip.ID, err)
	}
	return pgtx.Commit(ctx)
}

// End ends the trip with status (anything but Active) at the transaction time.
func (tx *Tx) End(status Status) error {
	if status == Active || status == "" {
		return fmt.Errorf("store: %q does not end a trip", status)
	}
	if tx.Trip.Status != Active {
		return fmt.Errorf("store: trip %s already ended (%s)", tx.Trip.ID, tx.Trip.Status)
	}
	tx.Trip.Status, tx.Trip.EndedAt = status, tx.now
	return nil
}

// Route returns one route of the locked trip.
func (tx *Tx) Route(ctx context.Context, version int) (*Route, error) {
	return scanRoute(tx.tx.QueryRow(ctx, `SELECT `+routeColumns+` FROM trip_routes
		WHERE trip_id = $1 AND version = $2`, tx.Trip.ID, version))
}

// AddRoute stores r as the next version of the trip's route, makes it the
// current one and returns its version.
func (tx *Tx) AddRoute(ctx context.Context, r Route) (int, error) {
	r.Version = tx.Trip.RouteVersion + 1
	if err := insertRoute(ctx, tx.tx, tx.Trip.ID, r, tx.now); err != nil {
		return 0, err
	}
	tx.Trip.RouteVersion = r.Version
	return r.Version, nil
}

// AddPositions stores positions and returns how many were new: a seq that
// is already stored (a batch sent again) is skipped.
func (tx *Tx) AddPositions(ctx context.Context, ps []Position) (int64, error) {
	if len(ps) == 0 {
		return 0, nil
	}
	n := len(ps)
	var (
		seq                   = make([]int64, n)
		devTime, recvTime     = make([]time.Time, n), make([]time.Time, n)
		lon, lat              = make([]float64, n), make([]float64, n)
		accuracy, speed, head = make([]*float32, n), make([]*float32, n), make([]*float32, n)
		version               = make([]int32, n)
		reject, class         = make([]*string, n), make([]*string, n)
		along, offset         = make([]*float64, n), make([]*float64, n)
	)
	for i, p := range ps {
		seq[i], devTime[i], recvTime[i], lon[i], lat[i] = p.Seq, p.DeviceTime, p.ReceivedAt, p.Pos.Lon, p.Pos.Lat
		version[i] = int32(p.RouteVersion)
		if p.Accuracy > 0 {
			accuracy[i] = ptr(float32(p.Accuracy))
		}
		if p.Speed >= 0 {
			speed[i] = ptr(float32(p.Speed))
		}
		if p.Heading >= 0 {
			head[i] = ptr(float32(p.Heading))
		}
		if p.Reject != "" {
			reject[i] = ptr(p.Reject)
			continue
		}
		class[i], along[i], offset[i] = ptr(string(p.Class)), ptr(p.Along), ptr(p.Offset)
	}
	tag, err := tx.tx.Exec(ctx, `
		INSERT INTO trip_positions (trip_id, seq, device_time, received_at, lon, lat, accuracy_m, speed_mps,
			heading, route_version, reject, class, along_m, offset_m)
		SELECT $1, * FROM unnest($2::bigint[], $3::timestamptz[], $4::timestamptz[], $5::float8[], $6::float8[],
			$7::real[], $8::real[], $9::real[], $10::integer[], $11::text[], $12::text[], $13::float8[], $14::float8[])
		ON CONFLICT (trip_id, seq) DO NOTHING`,
		tx.Trip.ID, seq, devTime, recvTime, lon, lat, accuracy, speed, head, version, reject, class, along, offset)
	if err != nil {
		return 0, fmt.Errorf("store: positions of trip %s: %w", tx.Trip.ID, err)
	}
	return tag.RowsAffected(), nil
}

func ptr[T any](v T) *T { return &v }

// AddEvent appends an event with data marshalled to JSON and returns its id.
// Listeners are notified when the transaction commits.
func (tx *Tx) AddEvent(ctx context.Context, typ string, data any) (int64, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("store: event %s: %w", typ, err)
	}
	var id int64
	err = tx.tx.QueryRow(ctx, `
		WITH ev AS (
			INSERT INTO trip_events (trip_id, type, data, created_at) VALUES ($1, $2, $3, $4) RETURNING id, trip_id
		)
		SELECT id, pg_notify($5, trip_id::text || ':' || id::text) FROM ev`,
		tx.Trip.ID, typ, b, tx.now, notifyChannel).Scan(&id, nil)
	if err != nil {
		return 0, fmt.Errorf("store: event %s of trip %s: %w", typ, tx.Trip.ID, err)
	}
	return id, nil
}

// ExpireIdle ends the active trips that have sent no positions since
// idleSince with status Expired, writes their EventStatus event and returns
// their ids. Trips locked by an Update at that moment are left for the next
// call: they are evidently not idle.
func (s *Store) ExpireIdle(ctx context.Context, idleSince time.Time) ([]string, error) {
	now := s.now()
	rows, _ := s.pool.Query(ctx, `
		WITH idle AS (
			SELECT id FROM trips WHERE status = 'active' AND last_seen_at < $1
			FOR UPDATE SKIP LOCKED
		), ended AS (
			UPDATE trips t SET status = 'expired', ended_at = $2, updated_at = $2
			FROM idle WHERE t.id = idle.id RETURNING t.id
		), ev AS (
			INSERT INTO trip_events (trip_id, type, data, created_at)
			SELECT id, $3, jsonb_build_object('status', 'expired'), $2 FROM ended RETURNING id, trip_id
		)
		SELECT trip_id::text, pg_notify($4, trip_id::text || ':' || id::text) FROM ev`,
		idleSince, now, EventStatus, notifyChannel)
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var id string
		err := row.Scan(&id, nil)
		return id, err
	})
}

// Purge deletes the trips that ended before `before`, with their routes,
// positions and events, and returns how many trips it deleted. It works in
// small transactions so that it never holds many locks for long.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM trips WHERE id IN (
			SELECT id FROM trips WHERE ended_at < $1 ORDER BY ended_at LIMIT 100)`, before)
		if err != nil {
			return total, fmt.Errorf("store: purge: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 100 {
			return total, nil
		}
	}
}

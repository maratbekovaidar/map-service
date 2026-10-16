package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"map-service/internal/clients/osrm"
	"map-service/internal/config"
	"map-service/internal/geo"
	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// CodeTripEnded is the error code for calls that need an active trip.
const CodeTripEnded = "trip_ended"

func tripEnded(st store.Status) *httpkit.Error {
	return &httpkit.Error{Status: http.StatusConflict, Code: CodeTripEnded, Message: fmt.Sprintf("the trip has ended (%s)", st)}
}

// Device clocks outside [received - maxFixAge, received + maxClockAhead]
// are wrong: such fixes are stored as rejected and never reach the tracker,
// whose ordering they would break.
const (
	maxClockAhead = 10 * time.Minute
	maxFixAge     = 24 * time.Hour
	rejectBadTime = "bad_time"
)

// decodeJSON reads a JSON body of at most limit bytes into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return httpkit.BadRequest("request body larger than %d bytes", limit)
		case errors.Is(err, io.EOF):
			return httpkit.BadRequest("request body must be a JSON object")
		}
		return httpkit.BadRequest("invalid JSON: %v", err)
	}
	if dec.More() {
		return httpkit.BadRequest("unexpected data after the JSON object")
	}
	return nil
}

func (s *Server) point(name string, ll *LatLon) (geo.Point, error) {
	if ll == nil {
		return geo.Point{}, httpkit.BadRequest("%s is required", name)
	}
	p := geo.Point{Lon: ll.Lon, Lat: ll.Lat}
	if !p.Valid() {
		return p, httpkit.BadRequest("%s is not a valid coordinate", name)
	}
	if !s.cfg.BBox.Contains(p) {
		return p, httpkit.BadRequest("%s is outside the service area (%s)", name, s.cfg.BBox)
	}
	return p, nil
}

func routeError(err error) error {
	if errors.Is(err, osrm.ErrNoRoute) {
		return httpkit.NoRoute()
	}
	return httpkit.Upstream("routing", err)
}

// POST /v1/trips
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) error {
	var req CreateTripRequest
	if err := decodeJSON(w, r, 4<<10, &req); err != nil {
		return err
	}
	origin, err := s.point("origin", req.Origin)
	if err != nil {
		return err
	}
	dest, err := s.point("destination", req.Destination)
	if err != nil {
		return err
	}
	heading := -1.0
	if req.Heading != nil {
		if *req.Heading < 0 || *req.Heading >= 360 || math.IsNaN(*req.Heading) {
			return httpkit.BadRequest("heading must be in [0, 360)")
		}
		heading = *req.Heading
	}

	route, err := s.buildRoute(r.Context(), origin, heading, dest)
	if err != nil {
		return routeError(err)
	}
	driverTok, driverHash, err := newToken(driver)
	if err != nil {
		return httpkit.Internal(err)
	}
	watchTok, watchHash, err := newToken(watcher)
	if err != nil {
		return httpkit.Internal(err)
	}
	trip, err := s.store.CreateTrip(r.Context(), store.NewTrip{
		Profile: config.TripsProfile, Origin: origin, Destination: dest,
		DriverTokenHash: driverHash, WatchTokenHash: watchHash, Route: route,
	})
	if err != nil {
		return httpkit.Internal(err)
	}
	route.Version, route.Reason, route.CreatedAt = 1, store.ReasonInitial, trip.CreatedAt
	httpkit.LoggerFrom(r.Context()).Info("trip created", "trip", trip.ID,
		"distance_m", math.Round(route.Distance), "duration_s", math.Round(route.Duration))
	httpkit.WriteJSON(w, http.StatusCreated, CreateTripResponse{
		Trip: tripDTO(trip), DriverToken: driverTok, WatchToken: watchTok, Route: routeDTO(&route),
	})
	return nil
}

// fixes turns the request into tracker fixes. Fixes whose device clock is
// off are returned separately, with the reason to store them under.
func (s *Server) fixes(req []PositionIn, received time.Time) (good []tracker.Fix, badTime []tracker.Fix, err error) {
	if len(req) == 0 {
		return nil, nil, httpkit.BadRequest("positions must not be empty")
	}
	if len(req) > s.cfg.MaxBatch {
		return nil, nil, httpkit.BadRequest("at most %d positions per request; send the rest in further requests", s.cfg.MaxBatch)
	}
	for i, p := range req {
		switch {
		case p.Seq < 1:
			return nil, nil, httpkit.BadRequest("positions[%d].seq must be >= 1", i)
		case p.TS.IsZero():
			return nil, nil, httpkit.BadRequest("positions[%d].ts is required (RFC 3339)", i)
		case p.Lat == nil || p.Lon == nil:
			return nil, nil, httpkit.BadRequest("positions[%d]: lat and lon are required", i)
		}
		f := tracker.Fix{Seq: p.Seq, Time: p.TS, Pos: geo.Point{Lon: *p.Lon, Lat: *p.Lat}, Speed: -1, Heading: -1}
		if p.Accuracy != nil && *p.Accuracy > 0 {
			f.Accuracy = *p.Accuracy
		}
		if p.Speed != nil && *p.Speed >= 0 {
			f.Speed = *p.Speed
		}
		if p.Heading != nil && *p.Heading >= 0 {
			f.Heading = geo.NormBearing(*p.Heading)
		}
		if p.TS.After(received.Add(maxClockAhead)) || p.TS.Before(received.Add(-maxFixAge)) {
			badTime = append(badTime, f)
			continue
		}
		good = append(good, f)
	}
	return good, badTime, nil
}

// batchResult is what the first transaction of a batch leaves for the rest.
type batchResult struct {
	update  tracker.Update
	status  store.Status
	version int
	lastSeq int64
	dest    geo.Point
}

// POST /v1/trips/{id}/positions
func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request, c caller) error {
	var req PositionsRequest
	if err := decodeJSON(w, r, int64(s.cfg.MaxBatch)*512+1024, &req); err != nil {
		return err
	}
	ctx := r.Context()
	good, badTime, err := s.fixes(req.Positions, s.now())
	if err != nil {
		return err
	}

	// 1. Follow the car and store everything, under the trip's lock.
	var res batchResult
	err = s.store.Update(ctx, c.trip.ID, func(ctx context.Context, tx *store.Tx) error {
		t := tx.Trip
		if t.Status != store.Active {
			return tripEnded(t.Status)
		}
		route, err := s.routes.get(t.ID, t.RouteVersion, func() (*store.Route, error) { return tx.Route(ctx, t.RouteVersion) })
		if err != nil {
			return err
		}
		upd := s.tracker.Process(ctx, route.tracker, &t.State, good, tx.Now())
		positions := make([]store.Position, 0, len(good)+len(badTime))
		accepted := make([]tracker.Fix, 0, len(good))
		bySeq := make(map[int64]tracker.Fix, len(good))
		for _, f := range good {
			bySeq[f.Seq] = f
		}
		for _, fr := range upd.Fixes {
			f := bySeq[fr.Seq]
			positions = append(positions, store.PositionOf(f, fr, t.RouteVersion, tx.Now()))
			if fr.Reject == "" {
				accepted = append(accepted, f)
			}
		}
		for _, f := range badTime {
			positions = append(positions, store.PositionOf(f, tracker.FixResult{Seq: f.Seq, Reject: rejectBadTime}, t.RouteVersion, tx.Now()))
		}
		if _, err := tx.AddPositions(ctx, positions); err != nil {
			return err
		}
		t.LastSeenAt = tx.Now()

		if len(accepted) > 0 && upd.Position != nil {
			if _, err := tx.AddEvent(ctx, EventPosition, positionEvent(accepted, upd, t.RouteVersion)); err != nil {
				return err
			}
		}
		if upd.Status == tracker.Arrived {
			if err := tx.End(store.Arrived); err != nil {
				return err
			}
			if _, err := tx.AddEvent(ctx, store.EventStatus, store.StatusEvent{Status: store.Arrived}); err != nil {
				return err
			}
		}
		res = batchResult{update: upd, status: t.Status, version: t.RouteVersion, lastSeq: t.State.LastSeq, dest: t.Destination}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return httpkit.NotFound("no such trip")
	}
	if err != nil {
		return err
	}
	log := httpkit.LoggerFrom(ctx)
	if res.status == store.Arrived {
		log.Info("trip arrived", "trip", c.trip.ID)
	}

	resp := PositionsResponse{Status: string(res.status), RouteVersion: res.version, LastSeq: res.lastSeq}
	if p := res.update.Position; p != nil {
		resp.Progress = &Progress{Along: p.Along, Remaining: p.RemainingDistance, RemainingTime: p.RemainingDuration,
			OffRoute: res.update.Status == tracker.OffRoute}
	}

	// 2. A new route, outside any transaction: OSRM may be slow.
	var current *store.Route
	if rr := res.update.Reroute; rr != nil && res.status == store.Active {
		current, err = s.reroute(ctx, c.trip.ID, res, rr)
		if err != nil {
			log.Warn("could not build a new route; the tracker will ask again", "trip", c.trip.ID, "reason", rr.Reason, "err", err)
		} else if current != nil {
			resp.RouteVersion = current.Version
			resp.Progress = &Progress{Remaining: current.Distance, RemainingTime: current.Duration}
			log.Info("trip rerouted", "trip", c.trip.ID, "version", current.Version, "reason", rr.Reason)
		}
	}
	if req.KnownRouteVersion != resp.RouteVersion {
		if current == nil {
			cr, err := s.routes.get(c.trip.ID, resp.RouteVersion, func() (*store.Route, error) {
				return s.routeOf(ctx, c.trip.ID, resp.RouteVersion)
			})
			if err != nil {
				return httpkit.Internal(err)
			}
			current = cr.stored
		}
		d := routeDTO(current)
		resp.Route = &d
	}
	httpkit.WriteJSON(w, http.StatusOK, resp)
	return nil
}

// reroute builds a route from where the car is and, if the trip is still on
// the route the request was judged against, makes it the current one. It
// returns nil when another request got there first or the trip ended.
func (s *Server) reroute(ctx context.Context, tripID string, res batchResult, rr *tracker.Reroute) (*store.Route, error) {
	route, err := s.buildRoute(ctx, rr.From, rr.Heading, res.dest)
	if err != nil {
		return nil, err
	}
	route.Reason = rr.Reason
	var stored *store.Route
	err = s.store.Update(ctx, tripID, func(ctx context.Context, tx *store.Tx) error {
		t := tx.Trip
		if t.Status != store.Active || t.RouteVersion != res.version {
			return nil
		}
		v, err := tx.AddRoute(ctx, route)
		if err != nil {
			return err
		}
		t.State.Rerouted(v)
		route.Version, route.CreatedAt = v, tx.Now()
		if _, err := tx.AddEvent(ctx, EventRoute, routeDTO(&route)); err != nil {
			return err
		}
		stored = &route
		return nil
	})
	return stored, err
}

// routeOf reads one route of a trip outside a transaction.
func (s *Server) routeOf(ctx context.Context, tripID string, version int) (*store.Route, error) {
	routes, err := s.store.Routes(ctx, tripID)
	if err != nil {
		return nil, err
	}
	for i := range routes {
		if routes[i].Version == version {
			return &routes[i], nil
		}
	}
	return nil, fmt.Errorf("trip %s has no route %d", tripID, version)
}

func positionEvent(accepted []tracker.Fix, upd tracker.Update, version int) PositionEvent {
	ev := PositionEvent{
		Position: fixDTO(upd.Position.Fix),
		Points:   make([]PointDTO, len(accepted)),
		Progress: Progress{Along: upd.Position.Along, Remaining: upd.Position.RemainingDistance,
			RemainingTime: upd.Position.RemainingDuration, OffRoute: upd.Status == tracker.OffRoute},
		RouteVersion: version,
	}
	for i, f := range accepted {
		ev.Points[i] = PointDTO{TS: f.Time.UTC(), Lat: f.Pos.Lat, Lon: f.Pos.Lon}
	}
	return ev
}

// GET /v1/trips/{id}
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, c caller) error {
	snap, err := s.store.Snapshot(r.Context(), c.trip.ID)
	if errors.Is(err, store.ErrNotFound) {
		return httpkit.NotFound("no such trip")
	}
	if err != nil {
		return httpkit.Internal(err)
	}
	resp := SnapshotResponse{Trip: tripDTO(snap.Trip), Route: routeDTO(snap.Route), LastEventID: snap.LastEventID}
	if snap.Position != nil {
		p := storedPositionDTO(snap.Position)
		resp.Position = &p
		route, err := s.routes.get(snap.Trip.ID, snap.Route.Version, func() (*store.Route, error) { return snap.Route, nil })
		if err != nil {
			return httpkit.Internal(err)
		}
		// Before the first fix on a new route its progress is zero.
		st := snap.Trip.State
		along := 0.0
		if st.RouteVersion == snap.Route.Version {
			along = st.Along
		}
		dist, secs := route.tracker.Remaining(along)
		resp.Progress = &Progress{Along: along, Remaining: dist, RemainingTime: secs, OffRoute: st.OffRoute}
	}
	httpkit.WriteJSON(w, http.StatusOK, resp)
	return nil
}

// POST /v1/trips/{id}/finish and /cancel. Ending an ended trip changes
// nothing and answers with how it ended, so a retried request is harmless.
func (s *Server) handleEnd(status store.Status) tripHandler {
	return func(w http.ResponseWriter, r *http.Request, c caller) error {
		var final store.Trip
		err := s.store.Update(r.Context(), c.trip.ID, func(ctx context.Context, tx *store.Tx) error {
			if tx.Trip.Status == store.Active {
				if err := tx.End(status); err != nil {
					return err
				}
				if _, err := tx.AddEvent(ctx, store.EventStatus, store.StatusEvent{Status: status}); err != nil {
					return err
				}
				httpkit.LoggerFrom(ctx).Info("trip ended", "trip", tx.Trip.ID, "status", status)
			}
			final = *tx.Trip
			return nil
		})
		if errors.Is(err, store.ErrNotFound) {
			return httpkit.NotFound("no such trip")
		}
		if err != nil {
			return httpkit.Internal(err)
		}
		httpkit.WriteJSON(w, http.StatusOK, EndResponse{Status: string(final.Status), EndedAt: final.EndedAt.UTC()})
		return nil
	}
}

package api

import (
	"time"

	"map-service/internal/geo"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// LatLon is a coordinate in requests and responses.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func latLon(p geo.Point) LatLon { return LatLon{Lat: p.Lat, Lon: p.Lon} }

// CreateTripRequest is the body of POST /v1/trips.
type CreateTripRequest struct {
	Origin      *LatLon `json:"origin"`
	Destination *LatLon `json:"destination"`
	// Heading is the direction the car is already moving in at the origin,
	// degrees clockwise from north; the route then starts that way.
	Heading *float64 `json:"heading"`
}

// TripDTO describes a trip.
type TripDTO struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	Origin      LatLon     `json:"origin"`
	Destination LatLon     `json:"destination"`
	CreatedAt   time.Time  `json:"created_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
}

func tripDTO(t *store.Trip) TripDTO {
	d := TripDTO{ID: t.ID, Status: string(t.Status), Origin: latLon(t.Origin), Destination: latLon(t.Destination),
		CreatedAt: t.CreatedAt.UTC()}
	if !t.EndedAt.IsZero() {
		ended := t.EndedAt.UTC()
		d.EndedAt = &ended
	}
	return d
}

// RouteDTO is one version of a trip's route. Geometry is an encoded
// polyline with precision 6 (decode with 1e6, like OSRM's polyline6).
type RouteDTO struct {
	Version   int       `json:"version"`
	Reason    string    `json:"reason"` // initial | off_route | wrong_way
	Distance  float64   `json:"distance_m"`
	Duration  float64   `json:"duration_s"`
	Geometry  string    `json:"geometry"`
	CreatedAt time.Time `json:"created_at"`
}

func routeDTO(r *store.Route) RouteDTO {
	return RouteDTO{Version: r.Version, Reason: r.Reason, Distance: r.Distance, Duration: r.Duration,
		Geometry: r.Geometry, CreatedAt: r.CreatedAt.UTC()}
}

// CreateTripResponse is the answer to POST /v1/trips. The tokens are shown
// this once: the service keeps only their hashes.
type CreateTripResponse struct {
	Trip        TripDTO  `json:"trip"`
	DriverToken string   `json:"driver_token"`
	WatchToken  string   `json:"watch_token"`
	Route       RouteDTO `json:"route"`
}

// PositionIn is one GPS fix the driver sends.
type PositionIn struct {
	Seq      int64     `json:"seq"` // from 1, increasing over the whole trip
	TS       time.Time `json:"ts"`  // device time, RFC 3339
	Lat      *float64  `json:"lat"`
	Lon      *float64  `json:"lon"`
	Accuracy *float64  `json:"accuracy"` // metres
	Speed    *float64  `json:"speed"`    // m/s
	Heading  *float64  `json:"heading"`  // degrees clockwise from north
}

// PositionsRequest is the body of POST /v1/trips/{id}/positions.
type PositionsRequest struct {
	// KnownRouteVersion is the route the app is showing; when the trip's
	// route is another one, the answer carries it.
	KnownRouteVersion int          `json:"known_route_version"`
	Positions         []PositionIn `json:"positions"`
}

// Progress is how far along its route the car is.
type Progress struct {
	Along         float64 `json:"along_m"`     // metres from the start of the route
	Remaining     float64 `json:"remaining_m"` // metres to the destination
	RemainingTime float64 `json:"remaining_s"` // seconds to the destination
	OffRoute      bool    `json:"off_route"`   // the car has left the route; a new one is on its way
}

// PositionsResponse is the answer to POST /v1/trips/{id}/positions.
type PositionsResponse struct {
	Status       string    `json:"status"`
	RouteVersion int       `json:"route_version"`
	LastSeq      int64     `json:"last_seq"` // highest seq the service has seen
	Progress     *Progress `json:"progress,omitempty"`
	Route        *RouteDTO `json:"route,omitempty"` // when route_version differs from known_route_version
}

// PositionDTO is a position as watchers see it.
type PositionDTO struct {
	Seq      int64     `json:"seq"`
	TS       time.Time `json:"ts"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Accuracy *float64  `json:"accuracy,omitempty"`
	Speed    *float64  `json:"speed,omitempty"`
	Heading  *float64  `json:"heading,omitempty"`
}

func positionDTO(seq int64, ts time.Time, p geo.Point, accuracy, speed, heading float64) PositionDTO {
	d := PositionDTO{Seq: seq, TS: ts.UTC(), Lat: p.Lat, Lon: p.Lon}
	if accuracy > 0 {
		d.Accuracy = &accuracy
	}
	if speed >= 0 {
		d.Speed = &speed
	}
	if heading >= 0 {
		d.Heading = &heading
	}
	return d
}

func fixDTO(f tracker.Fix) PositionDTO {
	return positionDTO(f.Seq, f.Time, f.Pos, f.Accuracy, f.Speed, f.Heading)
}

func storedPositionDTO(p *store.Position) PositionDTO {
	return positionDTO(p.Seq, p.DeviceTime, p.Pos, p.Accuracy, p.Speed, p.Heading)
}

// SnapshotResponse is the answer to GET /v1/trips/{id}: the trip as it is
// now. LastEventID is where its event stream continues.
type SnapshotResponse struct {
	Trip        TripDTO      `json:"trip"`
	Route       RouteDTO     `json:"route"`
	Position    *PositionDTO `json:"position,omitempty"`
	Progress    *Progress    `json:"progress,omitempty"`
	LastEventID int64        `json:"last_event_id"`
}

// EndResponse is the answer to POST /v1/trips/{id}/finish and /cancel.
type EndResponse struct {
	Status  string    `json:"status"`
	EndedAt time.Time `json:"ended_at"`
}

// Event types besides store.EventStatus.
const (
	EventPosition = "position" // data: PositionEvent
	EventRoute    = "route"    // data: RouteDTO
)

// PositionEvent is written for every batch with at least one accepted fix.
type PositionEvent struct {
	Position     PositionDTO `json:"position"` // the latest
	Points       []PointDTO  `json:"points"`   // the batch's accepted fixes, oldest first, to animate the car
	Progress     Progress    `json:"progress"`
	RouteVersion int         `json:"route_version"`
}

// PointDTO is a timestamped coordinate.
type PointDTO struct {
	TS  time.Time `json:"ts"`
	Lat float64   `json:"lat"`
	Lon float64   `json:"lon"`
}

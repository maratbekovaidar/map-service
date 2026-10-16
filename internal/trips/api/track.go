package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
)

const (
	trackPageDefault = 1000
	trackPageMax     = 5000
)

// TrackPosition is a stored position with what the tracker made of it.
type TrackPosition struct {
	PositionDTO
	ReceivedAt   time.Time `json:"received_at"`
	RouteVersion int       `json:"route_version"`
	Class        string    `json:"class,omitempty"`  // on | near | off | wrong_way | start
	Reject       string    `json:"reject,omitempty"` // why it was ignored
}

// TrackResponse is the answer to GET /v1/trips/{id}/track: the history of
// the trip, every route it had and its positions page by page.
type TrackResponse struct {
	Trip      TripDTO         `json:"trip"`
	Routes    []RouteDTO      `json:"routes"`
	Positions []TrackPosition `json:"positions"`
	// NextAfterSeq is set when there are more positions: ask again with
	// after_seq set to it.
	NextAfterSeq *int64 `json:"next_after_seq,omitempty"`
}

func intParam(r *http.Request, name string, def, lo, hi int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, httpkit.BadRequest("%s must be an integer between %d and %d", name, lo, hi)
	}
	return n, nil
}

// GET /v1/trips/{id}/track?after_seq=0&limit=1000
func (s *Server) handleTrack(w http.ResponseWriter, r *http.Request, c caller) error {
	after, err := intParam(r, "after_seq", 0, 0, 1<<62)
	if err != nil {
		return err
	}
	limit, err := intParam(r, "limit", trackPageDefault, 1, trackPageMax)
	if err != nil {
		return err
	}
	ctx := r.Context()
	routes, err := s.store.Routes(ctx, c.trip.ID)
	if errors.Is(err, store.ErrNotFound) {
		return httpkit.NotFound("no such trip")
	}
	if err != nil {
		return httpkit.Internal(err)
	}
	// One more than asked tells whether another page follows.
	ps, err := s.store.Positions(ctx, c.trip.ID, after, int(limit)+1)
	if err != nil {
		return httpkit.Internal(err)
	}
	resp := TrackResponse{Trip: tripDTO(c.trip), Routes: make([]RouteDTO, len(routes)), Positions: make([]TrackPosition, 0, len(ps))}
	for i := range routes {
		resp.Routes[i] = routeDTO(&routes[i])
	}
	if int64(len(ps)) > limit {
		ps = ps[:limit]
		next := ps[len(ps)-1].Seq
		resp.NextAfterSeq = &next
	}
	for i := range ps {
		p := &ps[i]
		resp.Positions = append(resp.Positions, TrackPosition{
			PositionDTO: storedPositionDTO(p), ReceivedAt: p.ReceivedAt.UTC(), RouteVersion: p.RouteVersion,
			Class: string(p.Class), Reject: p.Reject,
		})
	}
	httpkit.WriteJSON(w, http.StatusOK, resp)
	return nil
}

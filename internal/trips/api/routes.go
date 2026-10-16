package api

import (
	"container/list"
	"context"
	"errors"
	"math"
	"sync"

	"map-service/internal/clients/osrm"
	"map-service/internal/config"
	"map-service/internal/geo"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// buildRoute asks OSRM for a car route from `from` to `to`. A non-negative
// heading makes it start in that direction of travel; if no such route
// exists (a one-way dead end, say) it is retried without one.
func (s *Server) buildRoute(ctx context.Context, from geo.Point, heading float64, to geo.Point) (store.Route, error) {
	p := osrm.RouteParams{Profile: config.TripsProfile, Points: []geo.Point{from, to}, Geometry: osrm.Polyline6, Annotations: true}
	if heading >= 0 {
		p.Bearings = []osrm.Bearing{{Value: int(math.Round(heading)), Range: 45}, {}}
	}
	res, err := s.router.Route(ctx, p)
	if errors.Is(err, osrm.ErrNoRoute) && p.Bearings != nil {
		p.Bearings, heading = nil, -1
		res, err = s.router.Route(ctx, p)
	}
	if err != nil {
		return store.Route{}, err
	}
	r := res.Routes[0]
	return store.Route{
		From: from, Heading: heading, Distance: r.Distance, Duration: r.Duration,
		Geometry: r.Geometry.Encoded, SegDurations: r.SegmentDurations(), DataVersion: s.version(),
	}, nil
}

// cachedRoute is a route in both forms: as stored and as the tracker uses it.
type cachedRoute struct {
	stored  *store.Route
	tracker *tracker.Route
}

type routeKey struct {
	trip    string
	version int
}

// routeCache keeps the most recently used routes; a route version never
// changes once stored, so entries never go stale.
type routeCache struct {
	max int
	mu  sync.Mutex
	ll  *list.List // of *routeEntry, most recent first
	m   map[routeKey]*list.Element
}

type routeEntry struct {
	key   routeKey
	route *cachedRoute
}

func newRouteCache(max int) *routeCache {
	return &routeCache{max: max, ll: list.New(), m: map[routeKey]*list.Element{}}
}

// get returns version of trip's route, loading it with load on a miss.
func (c *routeCache) get(trip string, version int, load func() (*store.Route, error)) (*cachedRoute, error) {
	key := routeKey{trip, version}
	c.mu.Lock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		r := el.Value.(*routeEntry).route
		c.mu.Unlock()
		return r, nil
	}
	c.mu.Unlock()

	stored, err := load()
	if err != nil {
		return nil, err
	}
	tr, err := stored.Tracker()
	if err != nil {
		return nil, err
	}
	r := &cachedRoute{stored: stored, tracker: tr}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok { // loaded meanwhile by another request
		return el.Value.(*routeEntry).route, nil
	}
	c.m[key] = c.ll.PushFront(&routeEntry{key: key, route: r})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*routeEntry).key)
	}
	return r, nil
}

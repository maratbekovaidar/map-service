// Package api is the HTTP API of the trips service: a driver creates a trip
// and reports positions, the service follows the car along its route and
// rebuilds the route when the car leaves it, and a watcher holding the
// trip's watch token reads where the car is.
//
// Creating a trip takes an API key; every other call carries one of the
// trip's own tokens (Authorization: Bearer), which the response to the
// creation is the only place to get.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"map-service/internal/clients/osrm"
	"map-service/internal/config"
	"map-service/internal/geo"
	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// Router builds car routes; *osrm.Client is one.
type Router interface {
	Route(ctx context.Context, p osrm.RouteParams) (*osrm.RouteResponse, error)
	Health(ctx context.Context, pt geo.Point) error
}

// Deps is everything a Server needs.
type Deps struct {
	Cfg     *config.Trips
	Log     *slog.Logger
	Store   *store.Store
	Router  Router
	Tracker *tracker.Tracker
	// Version returns the live map data version (stored with every route).
	Version func() string
	// Now is the clock; nil means time.Now (tests inject a fake).
	Now func() time.Time
}

// Server is the trips service's HTTP handler.
type Server struct {
	cfg     *config.Trips
	log     *slog.Logger
	store   *store.Store
	router  Router
	tracker *tracker.Tracker
	version func() string
	now     func() time.Time

	auth    *httpkit.Authenticator
	keyLim  *httpkit.Limiter
	ipLim   *httpkit.Limiter
	tripLim *httpkit.Limiter
	tokLim  *httpkit.Limiter
	routes  *routeCache
}

// New builds a Server.
func New(d Deps) *Server {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	version := d.Version
	if version == nil {
		version = func() string { return "none" }
	}
	return &Server{
		cfg: d.Cfg, log: d.Log, store: d.Store, router: d.Router, tracker: d.Tracker, version: version, now: now,
		// Admin keys are valid client keys too, as in the gateway.
		auth:    httpkit.NewAuthenticator(append(append([]string{}, d.Cfg.APIKeys...), d.Cfg.AdminKeys...), d.Cfg.AuthDisabled),
		keyLim:  httpkit.NewLimiter(d.Cfg.LimitKey, now),
		ipLim:   httpkit.NewLimiter(d.Cfg.LimitIP, now),
		tripLim: httpkit.NewLimiter(d.Cfg.LimitTrip, now),
		tokLim:  httpkit.NewLimiter(d.Cfg.LimitTokenIP, now),
		routes:  newRouteCache(1000),
	}
}

// Handler returns the complete HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ip := func(r *http.Request) string { return httpkit.ClientIP(r, s.cfg.TrustedProxies) }
	wrap := httpkit.Wrap

	// Creating a trip: per-IP limit -> API key -> per-key limit.
	httpkit.Route(mux, "POST", "/v1/trips", httpkit.Chain(wrap(s.handleCreate),
		httpkit.LimitByIP(s.ipLim, ip), s.auth.Require, httpkit.LimitByKey(s.keyLim)))

	// Everything else: per-IP limit -> trip token (and the per-trip limit).
	trip := func(h tripHandler, roles ...role) http.Handler {
		return httpkit.Chain(wrap(s.withTrip(h, roles...)), httpkit.LimitByIP(s.tokLim, ip))
	}
	httpkit.Route(mux, "GET", "/v1/trips/{id}", trip(s.handleSnapshot, driver, watcher))
	httpkit.Route(mux, "POST", "/v1/trips/{id}/positions", trip(s.handlePositions, driver))
	httpkit.Route(mux, "POST", "/v1/trips/{id}/finish", trip(s.handleEnd(store.Finished), driver))
	httpkit.Route(mux, "POST", "/v1/trips/{id}/cancel", trip(s.handleEnd(store.Cancelled), driver))

	httpkit.Route(mux, "GET", "/healthz", wrap(s.handleHealthz))
	httpkit.Route(mux, "GET", "/readyz", wrap(s.handleReadyz))
	mux.Handle("/", wrap(func(w http.ResponseWriter, r *http.Request) error {
		return httpkit.NotFound("no such endpoint")
	}))

	cors := httpkit.CORS(s.cfg.CORSOrigins, "GET, POST, OPTIONS",
		"Authorization, X-API-Key, Content-Type, Last-Event-ID, X-Request-Id")
	return httpkit.Chain(mux, httpkit.Observe(s.log, s.now), cors, noStore)
}

// noStore: every answer is about one trip (positions, tokens) and must not
// be kept by caches along the way.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

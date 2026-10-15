package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"map-service/internal/cache"
	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/config"
	"map-service/internal/geo"
	"map-service/internal/styles"
	"map-service/internal/tiles"
)

// The gateway depends on its backends only through these interfaces; the real
// implementations are the clients in internal/clients.

// PhotonAPI is the autocomplete backend.
type PhotonAPI interface {
	Search(ctx context.Context, p photon.SearchParams) ([]photon.Feature, error)
	Health(ctx context.Context) error
}

// NominatimAPI is the geocoding backend.
type NominatimAPI interface {
	Search(ctx context.Context, p nominatim.SearchParams) ([]nominatim.Place, error)
	Reverse(ctx context.Context, lat, lon float64, languages []string) (*nominatim.Place, error)
	Health(ctx context.Context) error
}

// RouteAPI is the routing backend of one profile.
type RouteAPI interface {
	Route(ctx context.Context, p osrm.RouteParams) (*osrm.RouteResponse, error)
	Health(ctx context.Context, pt geo.Point) error
}

// TileSource reads vector tiles of a data version.
type TileSource interface {
	Tile(ctx context.Context, version string, z, x, y int) tiles.Result
	Ready(ctx context.Context, version string) error
}

// Deps is everything a Server needs.
type Deps struct {
	Cfg       *config.Gateway
	Log       *slog.Logger
	Photon    PhotonAPI
	Nominatim NominatimAPI
	OSRM      map[string]RouteAPI // by profile
	Cache     cache.Store
	Tiles     TileSource
	// Version returns the live data version (cache key prefix, /readyz).
	Version func() string
	// Now is the clock; nil means time.Now (tests inject a fake).
	Now func() time.Time
}

// Server is the gateway's HTTP handler.
type Server struct {
	cfg       *config.Gateway
	log       *slog.Logger
	photon    PhotonAPI
	nominatim NominatimAPI
	osrm      map[string]RouteAPI
	cache     cache.Store
	tiles     TileSource
	version   func() string
	now       func() time.Time

	val     *validator
	auth    *Authenticator
	admin   *Authenticator
	styles  *styles.Catalog
	ipLim   *Limiter
	keyLim  *Limiter
	tileLim *Limiter
	static  *staticFiles
	ready   readyCache
}

// New builds a Server.
func New(d Deps) *Server {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	store := d.Cache
	if store == nil {
		store = cache.Noop{}
	}
	version := d.Version
	if version == nil {
		version = func() string { return "none" }
	}
	s := &Server{
		cfg: d.Cfg, log: d.Log, photon: d.Photon, nominatim: d.Nominatim, osrm: d.OSRM,
		cache: store, tiles: d.Tiles, version: version, now: now,
		val: newValidator(d.Cfg),
		// Admin keys are valid client keys too, so one key works in the preview.
		auth:    NewAuthenticator(append(append([]string{}, d.Cfg.APIKeys...), d.Cfg.AdminKeys...), d.Cfg.AuthDisabled),
		admin:   NewAuthenticator(d.Cfg.AdminKeys, false),
		styles:  styles.New(filepath.Join(d.Cfg.StaticDir, "styles"), d.Cfg.StateDir, d.Cfg.DefaultStyle),
		ipLim:   NewLimiter(d.Cfg.LimitIP, now),
		keyLim:  NewLimiter(d.Cfg.LimitKey, now),
		tileLim: NewLimiter(d.Cfg.LimitTiles, now),
		static:  newStaticFiles(d.Cfg),
	}
	s.ready = readyCache{ttl: time.Second, now: now}
	if err := s.styles.Load(); err != nil && d.Log != nil {
		d.Log.Warn("ignoring the saved default style", "err", err)
	}
	return s
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// wrap turns an error-returning handler into an http.Handler that renders
// errors in the unified format and logs server-side failures.
func (s *Server) wrap(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		e := asError(err)
		if e.Status >= 500 && e.Status != statusClientClosed {
			loggerFrom(r.Context()).Error("request failed", "code", e.Code, "err", e.Err, "msg", e.Message)
		}
		writeError(w, e)
	})
}

// route registers h for pattern and answers any method other than method
// (HEAD is allowed wherever GET is) with a JSON 405. The method is checked
// here rather than in the mux pattern because ServeMux rejects overlapping
// "GET /tiles/{z}/{x}/{file}" and "/tiles/fonts/{stack}/{file}" style pairs.
func route(mux *http.ServeMux, method, pattern string, h http.Handler) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		headOnGet := method == http.MethodGet && r.Method == http.MethodHead
		if r.Method != method && !headOnGet {
			writeError(w, methodNotAllowed(method))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Handler returns the complete HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ip := func(r *http.Request) string { return clientIP(r, s.cfg.TrustedProxies) }

	// /v1/*: per-IP limit -> API key -> per-key limit.
	api := func(h handlerFunc) http.Handler {
		return chain(s.wrap(h), limitByIP(s.ipLim, ip), s.auth.Require, limitByKey(s.keyLim))
	}
	route(mux, "GET", "/v1/autocomplete", api(s.handleAutocomplete))
	route(mux, "GET", "/v1/geocode", api(s.handleGeocode))
	route(mux, "GET", "/v1/reverse", api(s.handleReverse))
	route(mux, "GET", "/v1/route", api(s.handleRoute))
	route(mux, "GET", "/v1/styles", api(s.handleStyles))
	// Switching the default style: per-IP limit -> admin key.
	route(mux, "PUT", "/v1/styles/default", chain(s.wrap(s.handleSetDefaultStyle), limitByIP(s.ipLim, ip), s.requireAdmin))

	// /tiles/*: its own (much higher) per-IP bucket; the key is optional
	// unless AUTH_REQUIRE_FOR_TILES is set.
	assets := func(h handlerFunc) http.Handler {
		mws := []func(http.Handler) http.Handler{limitByIP(s.tileLim, ip)}
		if s.cfg.AuthRequireForTiles {
			mws = append(mws, s.auth.Require)
		}
		return chain(s.wrap(h), mws...)
	}
	route(mux, "GET", "/tiles/style.json", assets(s.handleStyle))
	route(mux, "GET", "/tiles/styles/{file}", assets(s.handleStyleByID))
	route(mux, "GET", "/tiles/preview.html", assets(s.handlePreview))
	route(mux, "GET", "/tiles/fonts/{fontstack}/{file}", assets(s.handleFont))
	route(mux, "GET", "/tiles/sprites/{name}", assets(s.handleSprite))
	route(mux, "GET", "/tiles/vendor/{file}", assets(s.handleVendor))
	route(mux, "GET", "/tiles/{z}/{x}/{file}", assets(s.handleTile))

	// API documentation: public (the key is entered in the page itself), on
	// the same per-IP bucket as the static assets.
	docs := func(h handlerFunc) http.Handler { return chain(s.wrap(h), limitByIP(s.tileLim, ip)) }
	route(mux, "GET", "/docs", docs(s.handleDocs))
	route(mux, "GET", "/docs/{file}", docs(s.handleDocsAsset))
	route(mux, "GET", "/openapi.yaml", docs(s.handleOpenAPI))

	route(mux, "GET", "/healthz", s.wrap(s.handleHealthz))
	route(mux, "GET", "/readyz", s.wrap(s.handleReadyz))
	mux.Handle("/", s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		return notFound("no such endpoint")
	}))

	return chain(mux, observe(s.log, s.now), cors(s.cfg.CORSOrigins))
}

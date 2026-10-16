// Package config loads service configuration from environment variables.
// Every variable is documented in .env.example.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"map-service/internal/geo"
	"map-service/internal/styles"
)

// Getenv abstracts os.Getenv so tests can inject values.
type Getenv func(key string) string

// OS reads from the process environment.
var OS Getenv = os.Getenv

// City holds the parameters shared by every component.
type City struct {
	Name      string
	BBox      geo.BBox
	Center    geo.Point
	Profiles  []string
	Languages []string
}

// Limit is a token-bucket rate limit.
type Limit struct {
	RPS   float64
	Burst int
}

// CacheTTL groups the Redis TTLs.
type CacheTTL struct {
	Reverse      time.Duration
	Geocode      time.Duration
	Autocomplete time.Duration
	Route        time.Duration
}

// Gateway is the configuration of cmd/gateway.
type Gateway struct {
	City

	ListenAddr        string
	LogLevel          slog.Level
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	PublicBaseURL  string
	DataRoot       string
	StaticDir      string
	StateDir       string        // writable runtime state (the chosen default style); "" = memory only
	DefaultStyle   string        // style served at /tiles/style.json until an admin picks another
	DataVersionTTL time.Duration // how often the live data version is re-read
	TilesMaxAge    time.Duration // Cache-Control max-age of tiles

	APIKeys             []string
	AdminKeys           []string // may also switch the default style; valid for /v1 too
	AuthDisabled        bool
	AuthRequireForTiles bool
	CORSOrigins         []string
	TrustedProxies      []netip.Prefix
	LimitKey            Limit
	LimitIP             Limit
	LimitTiles          Limit

	TTL           CacheTTL
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	PhotonURL       string
	NominatimURL    string
	OSRMURLs        map[string]string // profile -> base URL
	UpstreamTimeout time.Duration
	RouteTimeout    time.Duration

	MaxResults     int
	MaxRoutePoints int
}

// loader accumulates parse errors so every problem is reported at once.
type loader struct {
	get  Getenv
	errs []error
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.get(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) list(key, def string) []string {
	return splitList(l.str(key, def))
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	s := strings.TrimSpace(l.get(key))
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: want a duration like 30s or 24h", key, s))
		return def
	}
	return d
}

func (l *loader) float(key string, def float64) float64 {
	s := strings.TrimSpace(l.get(key))
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: want a non-negative number", key, s))
		return def
	}
	return f
}

func (l *loader) int(key string, def int) int {
	s := strings.TrimSpace(l.get(key))
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: want a non-negative integer", key, s))
		return def
	}
	return n
}

func (l *loader) bool(key string, def bool) bool {
	s := strings.TrimSpace(l.get(key))
	if s == "" {
		return def
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q: want true or false", key, s))
		return def
	}
	return b
}

func (l *loader) limit(rpsKey, burstKey string, rps float64, burst int) Limit {
	return Limit{RPS: l.float(rpsKey, rps), Burst: l.int(burstKey, burst)}
}

// logLevel parses a debug|info|warn|error variable.
func (l *loader) logLevel(key string) slog.Level {
	switch strings.ToLower(l.str(key, "info")) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	l.fail("%s=%q: want debug, info, warn or error", key, l.get(key))
	return slog.LevelInfo
}

// trustedProxies parses TRUSTED_PROXY_CIDRS.
func (l *loader) trustedProxies() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range l.list("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.1/32") {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			l.fail("TRUSTED_PROXY_CIDRS: %q: %v", c, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// osrmURLs returns the OSRM base URL of every profile from OSRM_URL_TEMPLATE.
func (l *loader) osrmURLs(profiles ...string) map[string]string {
	tmpl := l.str("OSRM_URL_TEMPLATE", "http://osrm-%s:5000")
	if strings.Count(tmpl, "%s") != 1 {
		l.fail("OSRM_URL_TEMPLATE=%q must contain exactly one %%s (the profile)", tmpl)
		tmpl = "http://osrm-%s:5000"
	}
	out := make(map[string]string, len(profiles))
	for _, p := range profiles {
		out[p] = strings.TrimRight(fmt.Sprintf(tmpl, p), "/")
	}
	return out
}

// apiKeys parses API_KEYS, ADMIN_API_KEYS and AUTH_DISABLED.
func (l *loader) apiKeys() (keys, admin []string, disabled bool) {
	keys = l.list("API_KEYS", "")
	admin = l.list("ADMIN_API_KEYS", "")
	disabled = l.bool("AUTH_DISABLED", false)
	if len(keys) == 0 && !disabled {
		l.fail("API_KEYS is empty: set at least one key (or AUTH_DISABLED=true for local development only)")
	}
	return keys, admin, disabled
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) err() error { return errors.Join(l.errs...) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadCity parses the city-level parameters.
func LoadCity(get Getenv) (City, error) {
	l := &loader{get: get}
	c := l.city()
	return c, l.err()
}

func (l *loader) city() City {
	c := City{
		Name:      l.str("CITY_NAME", "Astana"),
		Profiles:  l.list("ROUTING_PROFILES", "car,foot"),
		Languages: l.list("LANGUAGES", "ru,kk,en"),
	}
	bbox, err := geo.ParseBBox(l.str("CITY_BBOX", "71.04,50.74,71.96,51.46"))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("CITY_BBOX: %w", err))
	}
	c.BBox = bbox
	centerStr := strings.TrimSpace(l.get("CITY_CENTER"))
	if centerStr == "" {
		c.Center = bbox.Center()
	} else if p, err := geo.ParsePoint(centerStr); err != nil {
		l.errs = append(l.errs, fmt.Errorf("CITY_CENTER: %w", err))
	} else {
		c.Center = p
		if bbox.Valid() && !bbox.Contains(p) {
			l.fail("CITY_CENTER %s is outside CITY_BBOX", centerStr)
		}
	}
	if len(c.Profiles) == 0 {
		l.fail("ROUTING_PROFILES must not be empty")
	}
	seen := map[string]bool{}
	for _, p := range c.Profiles {
		if !validIdent(p) {
			l.fail("ROUTING_PROFILES: invalid profile name %q", p)
		}
		if seen[p] {
			l.fail("ROUTING_PROFILES: duplicate profile %q", p)
		}
		seen[p] = true
	}
	if len(c.Languages) == 0 {
		l.fail("LANGUAGES must not be empty")
	}
	return c
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// LoadGateway parses the gateway configuration.
func LoadGateway(get Getenv) (*Gateway, error) {
	l := &loader{get: get}
	g := &Gateway{City: l.city()}

	g.ListenAddr = l.str("GATEWAY_LISTEN_ADDR", ":8080")
	g.LogLevel = l.logLevel("GATEWAY_LOG_LEVEL")
	g.ReadHeaderTimeout = l.dur("GATEWAY_READ_HEADER_TIMEOUT", 5*time.Second)
	g.ReadTimeout = l.dur("GATEWAY_READ_TIMEOUT", 15*time.Second)
	g.WriteTimeout = l.dur("GATEWAY_WRITE_TIMEOUT", 30*time.Second)
	g.IdleTimeout = l.dur("GATEWAY_IDLE_TIMEOUT", 60*time.Second)
	g.ShutdownTimeout = l.dur("GATEWAY_SHUTDOWN_TIMEOUT", 20*time.Second)

	g.PublicBaseURL = strings.TrimRight(l.str("PUBLIC_BASE_URL", ""), "/")
	g.DataRoot = l.str("DATA_ROOT", "/data")
	g.StaticDir = l.str("STATIC_DIR", "/app/static")
	g.StateDir = l.str("STATE_DIR", "")
	g.DefaultStyle = l.str("DEFAULT_STYLE", "liberty")
	if !styles.ValidID(g.DefaultStyle) {
		l.fail("DEFAULT_STYLE=%q: want a style id like liberty or dark-matter (lowercase letters, digits, '-')", g.DefaultStyle)
	}
	g.DataVersionTTL = l.dur("DATA_VERSION_POLL", 10*time.Second)

	g.APIKeys, g.AdminKeys, g.AuthDisabled = l.apiKeys()
	g.TilesMaxAge = l.dur("TILES_CACHE_MAX_AGE", time.Hour)
	g.AuthRequireForTiles = l.bool("AUTH_REQUIRE_FOR_TILES", false)
	g.CORSOrigins = l.list("CORS_ALLOWED_ORIGINS", "*")
	g.TrustedProxies = l.trustedProxies()
	g.LimitKey = l.limit("RATE_LIMIT_KEY_RPS", "RATE_LIMIT_KEY_BURST", 20, 40)
	g.LimitIP = l.limit("RATE_LIMIT_IP_RPS", "RATE_LIMIT_IP_BURST", 10, 20)
	g.LimitTiles = l.limit("RATE_LIMIT_TILES_RPS", "RATE_LIMIT_TILES_BURST", 200, 600)

	g.TTL = CacheTTL{
		Reverse:      l.dur("CACHE_TTL_REVERSE", 7*24*time.Hour),
		Geocode:      l.dur("CACHE_TTL_GEOCODE", 24*time.Hour),
		Autocomplete: l.dur("CACHE_TTL_AUTOCOMPLETE", 24*time.Hour),
		Route:        l.dur("CACHE_TTL_ROUTE", time.Hour),
	}
	g.RedisAddr = l.str("REDIS_ADDR", "")
	g.RedisPassword = l.str("REDIS_PASSWORD", "")
	g.RedisDB = l.int("REDIS_DB", 0)

	g.PhotonURL = strings.TrimRight(l.str("PHOTON_URL", "http://photon:2322"), "/")
	g.NominatimURL = strings.TrimRight(l.str("NOMINATIM_URL", "http://nominatim:8080"), "/")
	g.OSRMURLs = l.osrmURLs(g.Profiles...)
	g.UpstreamTimeout = l.dur("UPSTREAM_TIMEOUT", 5*time.Second)
	g.RouteTimeout = l.dur("ROUTE_TIMEOUT", 10*time.Second)

	g.MaxResults = l.int("MAX_RESULTS_LIMIT", 20)
	g.MaxRoutePoints = l.int("MAX_ROUTE_POINTS", 25)
	if g.MaxResults < 1 {
		l.fail("MAX_RESULTS_LIMIT must be >= 1")
	}
	if g.MaxRoutePoints < 2 {
		l.fail("MAX_ROUTE_POINTS must be >= 2")
	}
	if g.UpstreamTimeout == 0 || g.RouteTimeout == 0 {
		l.fail("UPSTREAM_TIMEOUT and ROUTE_TIMEOUT must be > 0")
	}
	if err := l.err(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return g, nil
}

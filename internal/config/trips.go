package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"
)

// TripsProfile is the routing profile trips are driven with.
const TripsProfile = "car"

// Trips is the configuration of cmd/trips, the trip tracking service.
type Trips struct {
	City

	ListenAddr string
	LogLevel   slog.Level
	// DatabaseURL is a postgres:// URL; empty means the standard PG*
	// variables, which compose sets for the trips-db service.
	DatabaseURL    string
	DataRoot       string        // the map data, for the version a route is built on
	DataVersionTTL time.Duration // how often that version is re-read

	APIKeys        []string // may create trips (as in the gateway)
	AdminKeys      []string // valid as API keys too
	AuthDisabled   bool
	CORSOrigins    []string
	TrustedProxies []netip.Prefix
	LimitKey       Limit // creating trips, per API key
	LimitIP        Limit // creating trips, per client IP
	LimitTrip      Limit // calls with a trip token, per trip and role
	LimitTokenIP   Limit // calls with a trip token, per client IP (mobile carriers put many drivers behind one)

	OSRMURL      string // the car profile
	RouteTimeout time.Duration

	MaxBatch    int           // positions per request
	IdleTimeout time.Duration // an active trip without positions for this long expires
	Retention   time.Duration // ended trips are deleted after this long
	// WatchGrace is how long after its end a trip's watch token still works
	// (the driver's token works until the trip is deleted).
	WatchGrace time.Duration
}

// LoadTrips parses the trips service configuration.
func LoadTrips(get Getenv) (*Trips, error) {
	l := &loader{get: get}
	t := &Trips{City: l.city()}

	t.ListenAddr = l.str("TRIPS_LISTEN_ADDR", ":8080")
	t.LogLevel = l.logLevel("TRIPS_LOG_LEVEL")
	t.DatabaseURL = l.str("TRIPS_DB_URL", "")
	t.DataRoot = l.str("DATA_ROOT", "/data")
	t.DataVersionTTL = l.dur("DATA_VERSION_POLL", 10*time.Second)

	t.APIKeys, t.AdminKeys, t.AuthDisabled = l.apiKeys()
	t.CORSOrigins = l.list("CORS_ALLOWED_ORIGINS", "*")
	t.TrustedProxies = l.trustedProxies()
	t.LimitKey = l.limit("RATE_LIMIT_KEY_RPS", "RATE_LIMIT_KEY_BURST", 20, 40)
	t.LimitIP = l.limit("RATE_LIMIT_IP_RPS", "RATE_LIMIT_IP_BURST", 10, 20)
	t.LimitTrip = l.limit("TRIPS_RATE_LIMIT_TRIP_RPS", "TRIPS_RATE_LIMIT_TRIP_BURST", 2, 10)
	t.LimitTokenIP = l.limit("TRIPS_RATE_LIMIT_IP_RPS", "TRIPS_RATE_LIMIT_IP_BURST", 50, 100)

	if !slices.Contains(t.Profiles, TripsProfile) {
		l.fail("ROUTING_PROFILES must contain %q: trips are driven by car", TripsProfile)
	}
	t.OSRMURL = l.osrmURLs(TripsProfile)[TripsProfile]
	t.RouteTimeout = l.dur("ROUTE_TIMEOUT", 10*time.Second)
	if t.RouteTimeout == 0 {
		l.fail("ROUTE_TIMEOUT must be > 0")
	}

	t.MaxBatch = l.int("TRIPS_MAX_BATCH", 100)
	if t.MaxBatch < 1 || t.MaxBatch > 1000 {
		l.fail("TRIPS_MAX_BATCH must be between 1 and 1000")
	}
	t.IdleTimeout = l.dur("TRIPS_IDLE_TIMEOUT", 30*time.Minute)
	if t.IdleTimeout < time.Minute {
		l.fail("TRIPS_IDLE_TIMEOUT must be at least 1m")
	}
	t.Retention = l.dur("TRIPS_RETENTION", 90*24*time.Hour)
	if t.Retention < 24*time.Hour {
		l.fail("TRIPS_RETENTION must be at least 24h")
	}
	t.WatchGrace = l.dur("TRIPS_WATCH_GRACE", time.Hour)
	if err := l.err(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return t, nil
}

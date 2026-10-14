package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestLoadGatewayRequiresAPIKeys(t *testing.T) {
	_, err := LoadGateway(env(nil))
	if err == nil || !strings.Contains(err.Error(), "API_KEYS is empty") {
		t.Fatalf("want an API_KEYS error, got %v", err)
	}
	if _, err := LoadGateway(env(map[string]string{"AUTH_DISABLED": "true"})); err != nil {
		t.Errorf("explicit AUTH_DISABLED must be accepted: %v", err)
	}
}

func TestLoadGatewayDefaults(t *testing.T) {
	g, err := LoadGateway(env(map[string]string{"API_KEYS": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "Astana" || g.BBox.String() != "71.04,50.74,71.96,51.46" {
		t.Errorf("city defaults: %+v", g.City)
	}
	// Without CITY_CENTER the bbox centre is used: (71.04+71.96)/2, (50.74+51.46)/2.
	if c := g.Center; c.Lon < 71.4999 || c.Lon > 71.5001 || c.Lat < 51.0999 || c.Lat > 51.1001 {
		t.Errorf("centre not derived from bbox: %+v", c)
	}
	if g.TTL.Reverse != 7*24*time.Hour || g.TTL.Geocode != 24*time.Hour || g.TTL.Route != time.Hour {
		t.Errorf("TTL defaults: %+v", g.TTL)
	}
	if got := g.OSRMURLs["car"]; got != "http://osrm-car:5000" {
		t.Errorf("OSRM car URL = %q", got)
	}
	if got := g.OSRMURLs["foot"]; got != "http://osrm-foot:5000" {
		t.Errorf("OSRM foot URL = %q", got)
	}
	if g.LogLevel != slog.LevelInfo || len(g.TrustedProxies) != 4 {
		t.Errorf("log/proxies: %v %v", g.LogLevel, g.TrustedProxies)
	}
	if g.TilesMaxAge != time.Hour || g.AuthDisabled || g.AuthRequireForTiles {
		t.Errorf("auth/tiles defaults: %+v", g)
	}
}

func TestLoadGatewayOverrides(t *testing.T) {
	g, err := LoadGateway(env(map[string]string{
		"CITY_NAME":            "Almaty",
		"CITY_BBOX":            "76.7,43.0,77.2,43.5",
		"CITY_CENTER":          "76.95,43.25",
		"ROUTING_PROFILES":     "car, foot ,bicycle",
		"API_KEYS":             "k1, k2",
		"CORS_ALLOWED_ORIGINS": "https://a.example,https://b.example",
		"CACHE_TTL_ROUTE":      "30m",
		"RATE_LIMIT_IP_RPS":    "2.5",
		"OSRM_URL_TEMPLATE":    "http://r-%s.internal:5000/",
		"GATEWAY_LOG_LEVEL":    "debug",
		"REDIS_ADDR":           "redis:6379",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "Almaty" || len(g.Profiles) != 3 || g.Profiles[2] != "bicycle" {
		t.Errorf("city: %+v", g.City)
	}
	if len(g.APIKeys) != 2 || g.APIKeys[1] != "k2" {
		t.Errorf("keys: %v", g.APIKeys)
	}
	if g.TTL.Route != 30*time.Minute || g.LimitIP.RPS != 2.5 || g.LogLevel != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v %+v", g.TTL, g.LimitIP)
	}
	if got := g.OSRMURLs["bicycle"]; got != "http://r-bicycle.internal:5000" {
		t.Errorf("template/trim: %q", got)
	}
}

func TestLoadGatewayReportsAllErrors(t *testing.T) {
	_, err := LoadGateway(env(map[string]string{
		"API_KEYS":          "k",
		"CITY_BBOX":         "1,2,3",
		"CACHE_TTL_ROUTE":   "soon",
		"RATE_LIMIT_IP_RPS": "-1",
		"ROUTING_PROFILES":  "car,car,Bad Name",
		"OSRM_URL_TEMPLATE": "http://osrm:5000",
		"MAX_ROUTE_POINTS":  "1",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"CITY_BBOX", "CACHE_TTL_ROUTE", "RATE_LIMIT_IP_RPS", "duplicate profile", "invalid profile name", "OSRM_URL_TEMPLATE", "MAX_ROUTE_POINTS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestCityCenterMustBeInsideBBox(t *testing.T) {
	_, err := LoadCity(env(map[string]string{"CITY_CENTER": "10,10"}))
	if err == nil || !strings.Contains(err.Error(), "outside CITY_BBOX") {
		t.Fatalf("err = %v", err)
	}
}

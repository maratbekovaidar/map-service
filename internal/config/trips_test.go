package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadTripsDefaults(t *testing.T) {
	c, err := LoadTrips(env(map[string]string{"API_KEYS": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DatabaseURL != "" || c.DataRoot != "/data" || c.OSRMURL != "http://osrm-car:5000" {
		t.Errorf("defaults: %+v", c)
	}
	if c.MaxBatch != 100 || c.IdleTimeout != 30*time.Minute || c.Retention != 90*24*time.Hour || c.RouteTimeout != 10*time.Second {
		t.Errorf("trip defaults: %+v", c)
	}
	if c.LimitTrip != (Limit{RPS: 2, Burst: 10}) || c.LimitTokenIP != (Limit{RPS: 50, Burst: 100}) || c.LimitKey != (Limit{RPS: 20, Burst: 40}) {
		t.Errorf("limits: %+v %+v %+v", c.LimitTrip, c.LimitTokenIP, c.LimitKey)
	}
	if len(c.TrustedProxies) != 4 || c.BBox.String() != "71.04,50.74,71.96,51.46" {
		t.Errorf("shared values: %+v", c)
	}
}

func TestLoadTripsOverridesAndErrors(t *testing.T) {
	c, err := LoadTrips(env(map[string]string{
		"API_KEYS": "k", "TRIPS_DB_URL": "postgres://u:p@db/trips", "OSRM_URL_TEMPLATE": "http://10.0.0.5:500%s/",
		"TRIPS_MAX_BATCH": "20", "TRIPS_IDLE_TIMEOUT": "10m", "TRIPS_RETENTION": "720h", "TRIPS_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://u:p@db/trips" || c.OSRMURL != "http://10.0.0.5:500car" || c.MaxBatch != 20 ||
		c.IdleTimeout != 10*time.Minute || c.Retention != 720*time.Hour {
		t.Errorf("overrides: %+v", c)
	}

	_, err = LoadTrips(env(map[string]string{
		"ROUTING_PROFILES": "foot", "TRIPS_MAX_BATCH": "0", "TRIPS_IDLE_TIMEOUT": "10s", "TRIPS_RETENTION": "1h",
		"TRIPS_LOG_LEVEL": "loud",
	}))
	for _, want := range []string{"API_KEYS is empty", `must contain "car"`, "TRIPS_MAX_BATCH", "TRIPS_IDLE_TIMEOUT", "TRIPS_RETENTION", "TRIPS_LOG_LEVEL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error mentioning %q, got %v", want, err)
		}
	}
}

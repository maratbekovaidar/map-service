package cache

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"map-service/internal/geo"
)

type payload struct {
	Name string  `json:"name"`
	N    float64 `json:"n"`
}

func newTestRedis(t *testing.T) (*Redis, *miniredis.Miniredis, *bytes.Buffer) {
	t.Helper()
	mr := miniredis.RunT(t)
	var logs bytes.Buffer
	r := NewRedis(Options{Addr: mr.Addr(), Backoff: time.Second}, slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { r.Close() })
	return r, mr, &logs
}

func TestRedisRoundTripAndTTL(t *testing.T) {
	r, mr, _ := newTestRedis(t)
	ctx := context.Background()
	var got payload
	if r.GetJSON(ctx, "k", &got) {
		t.Fatal("empty cache must miss")
	}
	r.SetJSON(ctx, "k", payload{Name: "Байтерек", N: 1.5}, time.Minute)
	if !r.GetJSON(ctx, "k", &got) || got.Name != "Байтерек" || got.N != 1.5 {
		t.Fatalf("hit expected, got %+v", got)
	}
	if ttl := mr.TTL("k"); ttl <= 0 || ttl > time.Minute {
		t.Errorf("TTL = %v", ttl)
	}
	mr.FastForward(2 * time.Minute)
	if r.GetJSON(ctx, "k", &got) {
		t.Error("entry must expire after its TTL")
	}
	if err := r.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestRedisCorruptEntryIsAMiss(t *testing.T) {
	r, mr, logs := newTestRedis(t)
	if err := mr.Set("bad", "{not json"); err != nil {
		t.Fatal(err)
	}
	var got payload
	if r.GetJSON(context.Background(), "bad", &got) {
		t.Error("corrupt entry must be a miss")
	}
	if !strings.Contains(logs.String(), "not valid JSON") {
		t.Errorf("expected a warning, logs: %s", logs.String())
	}
}

func TestRedisDownDegradesToMissWithBackoff(t *testing.T) {
	r, mr, logs := newTestRedis(t)
	now := time.Unix(1_000_000, 0)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	r.SetJSON(ctx, "k", payload{Name: "x"}, time.Minute)

	addr := mr.Addr()
	mr.Close() // Redis goes away

	var got payload
	if r.GetJSON(ctx, "k", &got) {
		t.Fatal("must miss while Redis is down")
	}
	if !strings.Contains(logs.String(), "redis unavailable") {
		t.Fatalf("a warning must be logged, logs: %s", logs.String())
	}
	logs.Reset()

	// During the backoff Redis is not contacted again and nothing is logged.
	start := time.Now()
	for i := 0; i < 50; i++ {
		if r.GetJSON(ctx, "k", &got) {
			t.Fatal("must keep missing")
		}
		r.SetJSON(ctx, "k", payload{}, time.Minute) // must not panic or block
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("calls during backoff must be instant, took %v", time.Since(start))
	}
	if logs.Len() != 0 {
		t.Errorf("no log spam during backoff, got: %s", logs.String())
	}
	if err := r.Ping(ctx); err == nil {
		t.Error("Ping must report the real (down) state")
	}

	// Redis returns; after the backoff the cache works again.
	mr2 := miniredis.NewMiniRedis()
	if err := mr2.StartAddr(addr); err != nil {
		t.Skipf("cannot restart miniredis on the same address: %v", err)
	}
	defer mr2.Close()
	now = now.Add(2 * time.Second)
	r.SetJSON(ctx, "k2", payload{Name: "back"}, time.Minute)
	if !r.GetJSON(ctx, "k2", &got) || got.Name != "back" {
		t.Errorf("cache should recover after backoff, got %+v", got)
	}
}

func TestNoopStore(t *testing.T) {
	var s Store = Noop{}
	var got payload
	s.SetJSON(context.Background(), "k", payload{Name: "x"}, time.Minute)
	if s.GetJSON(context.Background(), "k", &got) || s.Enabled() || s.Ping(context.Background()) != nil {
		t.Error("Noop must never cache and never fail")
	}
}

func TestKeys(t *testing.T) {
	const v1, v2 = "20261014T031500Z", "20261021T031500Z"

	// reverse: rounded to 5 decimals
	a := ReverseKey(v1, 51.128312345, 71.430698765, "ru")
	b := ReverseKey(v1, 51.12831, 71.4307, "ru")
	if a != b {
		t.Errorf("reverse keys must round to 5 decimals:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, "map:v1:"+v1+":reverse:51.12831,71.43070:") {
		t.Errorf("reverse key shape: %s", a)
	}
	if a == ReverseKey(v1, 51.12831, 71.4307, "kk") {
		t.Error("language must be part of the reverse key")
	}

	// data version isolates everything
	if ReverseKey(v1, 1, 2, "ru") == ReverseKey(v2, 1, 2, "ru") {
		t.Error("reverse key must contain the data version")
	}
	if GeocodeKey(v1, "q", 5, "ru") == GeocodeKey(v2, "q", 5, "ru") {
		t.Error("geocode key must contain the data version")
	}
	pts := []geo.Point{{Lon: 71.4, Lat: 51.1}, {Lon: 71.5, Lat: 51.2}}
	if RouteKey(v1, "car", pts, false, false, "polyline") == RouteKey(v2, "car", pts, false, false, "polyline") {
		t.Error("route key must contain the data version")
	}

	// geocode/autocomplete: normalized query
	if GeocodeKey(v1, "  Байтерек   Астана ", 5, "ru") != GeocodeKey(v1, "байтерек астана", 5, "ru") {
		t.Error("geocode key must normalize the query")
	}
	if GeocodeKey(v1, "q", 5, "ru") == GeocodeKey(v1, "q", 5, "kk") {
		t.Error("language must be part of the geocode key")
	}
	if GeocodeKey(v1, "q", 5, "ru") == GeocodeKey(v1, "q", 10, "ru") {
		t.Error("limit must be part of the geocode key")
	}
	lat, lon := 51.1283, 71.4306
	lat2, lon2 := 51.1290, 71.4311 // < 1 km away
	lat3 := 51.30
	if AutocompleteKey(v1, "q", 5, "ru", &lat, &lon) != AutocompleteKey(v1, "q", 5, "ru", &lat2, &lon2) {
		t.Error("nearby locations should share an autocomplete entry")
	}
	if AutocompleteKey(v1, "q", 5, "ru", &lat, &lon) == AutocompleteKey(v1, "q", 5, "ru", &lat3, &lon) {
		t.Error("distant locations must not share an autocomplete entry")
	}
	if AutocompleteKey(v1, "q", 5, "ru", nil, nil) == AutocompleteKey(v1, "q", 5, "ru", &lat, &lon) {
		t.Error("biased and unbiased autocomplete must differ")
	}

	// route: 5-decimal rounding, options
	pts2 := []geo.Point{{Lon: 71.400001, Lat: 51.099999}, {Lon: 71.5, Lat: 51.2}}
	if RouteKey(v1, "car", pts, false, false, "polyline") != RouteKey(v1, "car", pts2, false, false, "polyline") {
		t.Error("route key must round points to 5 decimals")
	}
	base := RouteKey(v1, "car", pts, false, false, "polyline")
	for name, k := range map[string]string{
		"profile": RouteKey(v1, "foot", pts, false, false, "polyline"),
		"alt":     RouteKey(v1, "car", pts, true, false, "polyline"),
		"steps":   RouteKey(v1, "car", pts, false, true, "polyline"),
		"geom":    RouteKey(v1, "car", pts, false, false, "geojson"),
	} {
		if k == base {
			t.Errorf("%s must be part of the route key", name)
		}
	}
}

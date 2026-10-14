package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
)

func TestReverseIsCachedByRoundedCoordinates(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.nom.reverse = func(context.Context, float64, float64, []string) (*nominatim.Place, error) {
		p := khanShatyrNominatim()
		return &p, nil
	}
	first := hs.get("/v1/reverse?lat=51.132501&lon=71.403901")
	// Differs only beyond the 5th decimal: same cache entry.
	second := hs.get("/v1/reverse?lat=51.1325012&lon=71.4039008")
	if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() {
		t.Fatalf("first=%d second=%d\n%s\n%s", first.Code, second.Code, first.Body, second.Body)
	}
	if n := hs.nom.revCalls.Load(); n != 1 {
		t.Errorf("upstream calls = %d, want 1 (second answer must come from the cache)", n)
	}
	// A different language is a different entry.
	hs.get("/v1/reverse?lat=51.132501&lon=71.403901&lang=kk")
	if n := hs.nom.revCalls.Load(); n != 2 {
		t.Errorf("language must be part of the key; upstream calls = %d", n)
	}

	keys := mr.Keys()
	if len(keys) != 2 {
		t.Fatalf("keys: %v", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "map:v1:20261014T000000Z:reverse:51.13250,71.40390:") {
			t.Errorf("unexpected key %q", k)
		}
		if ttl := mr.TTL(k); ttl != 7*24*time.Hour {
			t.Errorf("reverse TTL = %v, want 7 days", ttl)
		}
	}
}

func TestGeocodeCacheNormalisesQueryAndUsesTTL(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return []nominatim.Place{khanShatyrNominatim()}, nil
	}
	hs.get("/v1/geocode?q=%D0%A5%D0%B0%D0%BD+%D0%A8%D0%B0%D1%82%D1%8B%D1%80")
	hs.get("/v1/geocode?q=++%D1%85%D0%B0%D0%BD++%D1%88%D0%B0%D1%82%D1%8B%D1%80+") // lower case, extra spaces
	if n := hs.nom.searchCalls.Load(); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
	for _, k := range mr.Keys() {
		if ttl := mr.TTL(k); ttl != 24*time.Hour {
			t.Errorf("geocode TTL = %v, want 1 day", ttl)
		}
	}
}

func TestAutocompleteCache(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		return []photon.Feature{khanShatyrPhoton()}, nil
	}
	hs.get("/v1/autocomplete?q=%D1%85%D0%B0%D0%BD")
	hs.get("/v1/autocomplete?q=%D0%A5%D0%B0%D0%9D")
	if n := hs.photon.calls.Load(); n != 1 {
		t.Errorf("photon calls = %d, want 1", n)
	}
	for _, k := range mr.Keys() {
		if ttl := mr.TTL(k); ttl != 24*time.Hour {
			t.Errorf("autocomplete TTL = %v", ttl)
		}
	}
}

// A fallback caused by a Photon *failure* must not be cached, otherwise a
// short outage would pin lower-quality Nominatim answers for a whole day.
func TestFallbackAfterPhotonFailureIsNotCached(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	photonDown := true
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		if photonDown {
			return nil, errors.New("connection refused")
		}
		return []photon.Feature{khanShatyrPhoton()}, nil
	}
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return []nominatim.Place{khanShatyrNominatim()}, nil
	}
	if rec := hs.get("/v1/autocomplete?q=x"); !strings.Contains(rec.Body.String(), `"source":"nominatim"`) {
		t.Fatalf("expected the fallback: %s", rec.Body)
	}
	if len(mr.Keys()) != 0 {
		t.Errorf("fallback-after-failure was cached: %v", mr.Keys())
	}
	photonDown = false
	if rec := hs.get("/v1/autocomplete?q=x"); !strings.Contains(rec.Body.String(), `"source":"photon"`) {
		t.Errorf("after Photon recovered the answer must come from Photon: %s", rec.Body)
	}
	if len(mr.Keys()) != 1 {
		t.Errorf("the healthy answer should be cached: %v", mr.Keys())
	}
}

func TestRouteCacheKeyedByOptionsAndProfile(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) { return simpleRoute(), nil }
	hs.foot.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) { return simpleRoute(), nil }
	base := "&points=71.4306,51.1283;71.47,51.18"

	hs.get("/v1/route?profile=car" + base)
	hs.get("/v1/route?profile=car" + base) // hit
	if hs.car.calls.Load() != 1 {
		t.Errorf("identical route must be served from cache, car calls = %d", hs.car.calls.Load())
	}
	hs.get("/v1/route?profile=foot" + base)
	hs.get("/v1/route?profile=car&steps=true" + base)
	hs.get("/v1/route?profile=car&geometry=geojson" + base)
	hs.get("/v1/route?profile=car&alternatives=true" + base)
	if hs.foot.calls.Load() != 1 || hs.car.calls.Load() != 4 {
		t.Errorf("profile and options must separate entries: car=%d foot=%d", hs.car.calls.Load(), hs.foot.calls.Load())
	}
	for _, k := range mr.Keys() {
		if ttl := mr.TTL(k); ttl != time.Hour {
			t.Errorf("route TTL = %v, want 1 hour (key %s)", ttl, k)
		}
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, errors.New("boom")
	}
	hs.get("/v1/geocode?q=x")
	hs.get("/v1/reverse?lat=51.1&lon=71.4") // default fake: not found
	hs.get("/v1/route?profile=car&points=71.4,51.1;71.5,51.2")
	if len(mr.Keys()) != 0 {
		t.Errorf("errors must never be cached: %v", mr.Keys())
	}
}

// After a data rebuild the version prefix changes, so old entries are not served.
func TestCacheKeysFollowTheDataVersion(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return []nominatim.Place{khanShatyrNominatim()}, nil
	}
	hs.get("/v1/geocode?q=x")
	hs.get("/v1/geocode?q=x")
	if hs.nom.searchCalls.Load() != 1 {
		t.Fatal("second request should hit the cache")
	}
	hs.version.Store("20261021T000000Z") // update-data switched the live version
	hs.get("/v1/geocode?q=x")
	if hs.nom.searchCalls.Load() != 2 {
		t.Errorf("a new data version must not reuse old cache entries; upstream calls = %d", hs.nom.searchCalls.Load())
	}
	var old, fresh int
	for _, k := range mr.Keys() {
		switch {
		case strings.HasPrefix(k, "map:v1:20261014T000000Z:"):
			old++
		case strings.HasPrefix(k, "map:v1:20261021T000000Z:"):
			fresh++
		}
	}
	if old != 1 || fresh != 1 {
		t.Errorf("old=%d fresh=%d keys=%v", old, fresh, mr.Keys())
	}
}

// Redis going away must not break any endpoint: every request is answered
// from the upstream, and a warning is logged.
func TestRedisUnavailableGatewayKeepsWorking(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return []nominatim.Place{khanShatyrNominatim()}, nil
	}
	hs.nom.reverse = func(context.Context, float64, float64, []string) (*nominatim.Place, error) {
		p := khanShatyrNominatim()
		return &p, nil
	}
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		return []photon.Feature{khanShatyrPhoton()}, nil
	}
	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) { return simpleRoute(), nil }

	mr.Close() // Redis dies

	start := time.Now()
	for _, target := range []string{
		"/v1/geocode?q=x", "/v1/reverse?lat=51.13&lon=71.4", "/v1/autocomplete?q=x",
		"/v1/route?profile=car&points=71.4,51.1;71.5,51.2",
		"/v1/geocode?q=x", "/v1/reverse?lat=51.13&lon=71.4", // repeated: still upstream, no cache
	} {
		if rec := hs.get(target); rec.Code != 200 {
			t.Errorf("%s: status %d: %s", target, rec.Code, rec.Body)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a dead Redis slowed requests down: %v", d)
	}
	if n := hs.nom.searchCalls.Load(); n != 2 {
		t.Errorf("without cache each geocode must reach the upstream, calls = %d", n)
	}
}

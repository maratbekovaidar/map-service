package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
)

// ---------------------------------------------------------------------- auth

func TestAuthentication(t *testing.T) {
	hs := newHarness(t, map[string]string{"API_KEYS": "alpha, beta"}, nil)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) { return nil, nil }

	for _, tc := range []struct {
		name    string
		headers []string
		want    int
	}{
		{"no key", nil, 401},
		{"wrong key", []string{"X-API-Key", "gamma"}, 401},
		{"empty key", []string{"X-API-Key", ""}, 401},
		{"prefix of a valid key", []string{"X-API-Key", "alph"}, 401},
		{"first key", []string{"X-API-Key", "alpha"}, 200},
		{"second key", []string{"X-API-Key", "beta"}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := hs.do("GET", "/v1/geocode?q=x", tc.headers...)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if tc.want == 401 {
				wantError(t, rec, 401, CodeUnauthorized)
			}
		})
	}
	// The key is never logged in clear text.
	hs.do("GET", "/v1/geocode?q=x", "X-API-Key", "alpha")
	if strings.Contains(hs.logs.String(), "alpha") {
		t.Error("API keys must not appear in logs")
	}
	if !strings.Contains(hs.logs.String(), `"key_id"`) {
		t.Error("the access log should carry a key_id")
	}
}

func TestPublicEndpointsNeedNoKey(t *testing.T) {
	hs := newHarness(t, nil, nil)
	for _, target := range []string{"/healthz", "/tiles/0/0/0.mvt"} {
		if rec := hs.do("GET", target); rec.Code >= 400 {
			t.Errorf("%s without a key: %d %s", target, rec.Code, rec.Body)
		}
	}
	if rec := hs.do("GET", "/v1/geocode?q=x"); rec.Code != 401 {
		t.Errorf("/v1 must require the key, got %d", rec.Code)
	}
}

func TestTilesRequireKeyWhenConfigured(t *testing.T) {
	hs := newHarness(t, map[string]string{"AUTH_REQUIRE_FOR_TILES": "true"}, nil)
	wantError(t, hs.do("GET", "/tiles/0/0/0.mvt"), 401, CodeUnauthorized)
	wantError(t, hs.do("GET", "/tiles/style.json"), 401, CodeUnauthorized)
	if rec := hs.get("/tiles/0/0/0.mvt"); rec.Code != http.StatusNoContent {
		t.Errorf("with a key: %d", rec.Code)
	}
	if rec := hs.do("GET", "/healthz"); rec.Code != 200 {
		t.Errorf("/healthz must stay public: %d", rec.Code)
	}
}

func TestAuthDisabledForLocalDevelopment(t *testing.T) {
	hs := newHarness(t, map[string]string{"API_KEYS": "", "AUTH_DISABLED": "true"}, nil)
	if rec := hs.do("GET", "/v1/geocode?q=x"); rec.Code != 200 {
		t.Errorf("AUTH_DISABLED must let requests through: %d %s", rec.Code, rec.Body)
	}
}

// ------------------------------------------------------------ rate limiting

func TestRateLimitPerIP(t *testing.T) {
	hs := newHarness(t, map[string]string{"RATE_LIMIT_IP_RPS": "1", "RATE_LIMIT_IP_BURST": "3"}, nil)
	var codes []int
	for i := 0; i < 5; i++ {
		codes = append(codes, hs.get("/v1/geocode?q=x").Code)
	}
	want := []int{200, 200, 200, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes = %v, want %v", codes, want)
		}
	}
	rec := hs.get("/v1/geocode?q=x")
	wantError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 || ra > 2 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	// Another client address has its own bucket.
	req := httptest.NewRequest("GET", "/v1/geocode?q=x", nil)
	req.RemoteAddr = "198.51.100.9:1111"
	req.Header.Set("X-API-Key", testKey)
	other := httptest.NewRecorder()
	hs.h.ServeHTTP(other, req)
	if other.Code != 200 {
		t.Errorf("a different IP must not be limited: %d", other.Code)
	}
	// Public probes are never rate limited.
	for i := 0; i < 20; i++ {
		if rec := hs.do("GET", "/healthz"); rec.Code != 200 {
			t.Fatalf("healthz limited at request %d", i)
		}
	}
}

func TestRateLimitPerKey(t *testing.T) {
	hs := newHarness(t, map[string]string{"API_KEYS": "alpha,beta", "RATE_LIMIT_KEY_RPS": "1", "RATE_LIMIT_KEY_BURST": "2"}, nil)
	call := func(key, ip string) int {
		req := httptest.NewRequest("GET", "/v1/geocode?q=x", nil)
		req.RemoteAddr = ip + ":1"
		req.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		return rec.Code
	}
	// The same key from different IPs shares one bucket...
	got := []int{call("alpha", "203.0.113.1"), call("alpha", "203.0.113.2"), call("alpha", "203.0.113.3")}
	if got[0] != 200 || got[1] != 200 || got[2] != 429 {
		t.Fatalf("alpha: %v", got)
	}
	// ...and another key is independent.
	if c := call("beta", "203.0.113.1"); c != 200 {
		t.Errorf("beta must have its own bucket: %d", c)
	}
}

func TestRateLimitTilesIsSeparate(t *testing.T) {
	hs := newHarness(t, map[string]string{
		"RATE_LIMIT_IP_RPS": "1", "RATE_LIMIT_IP_BURST": "1",
		"RATE_LIMIT_TILES_RPS": "1", "RATE_LIMIT_TILES_BURST": "50"}, nil)
	for i := 0; i < 30; i++ { // a map view fetches many tiles at once
		if rec := hs.do("GET", "/tiles/0/0/0.mvt"); rec.Code != http.StatusNoContent {
			t.Fatalf("tile %d: %d", i, rec.Code)
		}
	}
	hs.get("/v1/geocode?q=x")
	if rec := hs.get("/v1/geocode?q=x"); rec.Code != 429 {
		t.Errorf("tile traffic must not use up the /v1 bucket nor vice versa; /v1 second call = %d", rec.Code)
	}
}

func TestRateLimitUsesForwardedClientBehindProxy(t *testing.T) {
	hs := newHarness(t, map[string]string{"RATE_LIMIT_IP_RPS": "1", "RATE_LIMIT_IP_BURST": "1"}, nil)
	call := func(xff string) int {
		req := httptest.NewRequest("GET", "/v1/geocode?q=x", nil)
		req.RemoteAddr = "172.20.0.5:9999" // the Caddy container
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("X-API-Key", testKey)
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if call("198.51.100.1") != 200 || call("198.51.100.1") != 429 {
		t.Error("the same real client must be limited across requests")
	}
	if call("198.51.100.2") != 200 {
		t.Error("different real clients behind one proxy must not share a bucket")
	}
}

// ---------------------------------------------------------------------- CORS

func TestCORS(t *testing.T) {
	hs := newHarness(t, map[string]string{"CORS_ALLOWED_ORIGINS": "https://app.example.com, https://admin.example.com"}, nil)

	rec := hs.get("/v1/geocode?q=x", "Origin", "https://app.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("allowed origin: %q", got)
	}
	if rec.Header().Get("Vary") != "Origin" || !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "ETag") {
		t.Errorf("headers: %v", rec.Header())
	}
	if got := hs.get("/v1/geocode?q=x", "Origin", "https://evil.example.com").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a foreign origin must get no CORS headers, got %q", got)
	}
	// CORS headers also accompany errors, otherwise browsers cannot read them.
	if got := hs.do("GET", "/v1/geocode?q=x", "Origin", "https://app.example.com").Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("401 responses must carry CORS headers")
	}

	// Preflight needs no API key.
	pre := hs.do("OPTIONS", "/v1/route", "Origin", "https://app.example.com",
		"Access-Control-Request-Method", "GET", "Access-Control-Request-Headers", "x-api-key")
	if pre.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", pre.Code)
	}
	if !strings.Contains(pre.Header().Get("Access-Control-Allow-Headers"), "X-API-Key") || pre.Header().Get("Access-Control-Allow-Methods") == "" || pre.Header().Get("Access-Control-Max-Age") == "" {
		t.Errorf("preflight headers: %v", pre.Header())
	}
	bad := hs.do("OPTIONS", "/v1/route", "Origin", "https://evil.example.com", "Access-Control-Request-Method", "GET")
	if bad.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("preflight from a foreign origin must not be approved")
	}
}

func TestCORSWildcard(t *testing.T) {
	hs := newHarness(t, map[string]string{"CORS_ALLOWED_ORIGINS": "*"}, nil)
	rec := hs.get("/healthz", "Origin", "https://anything.example")
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Vary") != "" {
		t.Errorf("wildcard: %v", rec.Header())
	}
	if hs.get("/healthz").Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no Origin header, no CORS headers")
	}
}

// --------------------------------------------------- request id / recovery / 404

func TestRequestID(t *testing.T) {
	hs := newHarness(t, nil, nil)
	rec := hs.get("/healthz")
	id := rec.Header().Get("X-Request-Id")
	if len(id) != 16 {
		t.Errorf("generated id = %q", id)
	}
	if got := hs.get("/healthz", "X-Request-Id", "client-supplied-123").Header().Get("X-Request-Id"); got != "client-supplied-123" {
		t.Errorf("a sane client id must be kept, got %q", got)
	}
	if got := hs.get("/healthz", "X-Request-Id", "bad id\"with{junk}").Header().Get("X-Request-Id"); got == "bad id\"with{junk}" || len(got) != 16 {
		t.Errorf("an unsafe client id must be replaced, got %q", got)
	}
}

func TestEveryLogLineCarriesTheRequestID(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, context.DeadlineExceeded
	}
	rec := hs.get("/v1/geocode?q=secret-address", "X-Request-Id", "req-42")
	if rec.Code != 504 {
		t.Fatalf("status %d", rec.Code)
	}
	lines := strings.Split(strings.TrimSpace(hs.logs.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the failure log and the access log, got:\n%s", hs.logs)
	}
	for _, l := range lines {
		if !strings.Contains(l, `"request_id":"req-42"`) {
			t.Errorf("log line without request id: %s", l)
		}
	}
	if strings.Contains(hs.logs.String(), "secret-address") {
		t.Error("query strings (user-typed addresses) must not be logged")
	}
	if !strings.Contains(hs.logs.String(), `"status":504`) || !strings.Contains(hs.logs.String(), `"path":"/v1/geocode"`) {
		t.Errorf("access log fields missing:\n%s", hs.logs)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) { panic("kaboom") }
	rec := hs.get("/v1/route?profile=car&points=71.4,51.1;71.5,51.2", "X-Request-Id", "req-panic")
	e := wantError(t, rec, http.StatusInternalServerError, CodeInternal)
	if strings.Contains(e.Message, "kaboom") {
		t.Error("the panic value must not leak")
	}
	if !strings.Contains(hs.logs.String(), "kaboom") || !strings.Contains(hs.logs.String(), "stack") {
		t.Errorf("the panic and its stack must be logged:\n%s", hs.logs)
	}
	// The server keeps serving afterwards.
	if hs.get("/healthz").Code != 200 {
		t.Error("server unusable after a panic")
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	hs := newHarness(t, nil, nil)
	wantError(t, hs.get("/nope"), http.StatusNotFound, CodeNotFound)
	wantError(t, hs.get("/v1/unknown"), http.StatusNotFound, CodeNotFound)

	for _, target := range []string{"/v1/geocode?q=x", "/v1/route", "/healthz", "/tiles/0/0/0.mvt", "/tiles/style.json"} {
		rec := hs.do("POST", target, "X-API-Key", testKey)
		wantError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
		if rec.Header().Get("Allow") == "" {
			t.Errorf("%s: missing Allow header", target)
		}
	}
	// HEAD works wherever GET does.
	if rec := hs.do("HEAD", "/healthz"); rec.Code != 200 {
		t.Errorf("HEAD /healthz = %d", rec.Code)
	}
}

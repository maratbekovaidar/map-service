package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"map-service/internal/cache"
	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/config"
	"map-service/internal/geo"
	"map-service/internal/tiles"
)

const testKey = "test-key"

// fakePhoton / fakeNominatim / fakeRoute are scriptable upstreams.
type fakePhoton struct {
	search func(context.Context, photon.SearchParams) ([]photon.Feature, error)
	health func(context.Context) error
	calls  atomic.Int32
	last   atomic.Pointer[photon.SearchParams]
}

func (f *fakePhoton) Search(ctx context.Context, p photon.SearchParams) ([]photon.Feature, error) {
	f.calls.Add(1)
	f.last.Store(&p)
	if f.search == nil {
		return nil, nil
	}
	return f.search(ctx, p)
}

func (f *fakePhoton) Health(ctx context.Context) error {
	if f.health == nil {
		return nil
	}
	return f.health(ctx)
}

type fakeNominatim struct {
	search      func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error)
	reverse     func(context.Context, float64, float64, []string) (*nominatim.Place, error)
	health      func(context.Context) error
	searchCalls atomic.Int32
	revCalls    atomic.Int32
	lastSearch  atomic.Pointer[nominatim.SearchParams]
	lastRevLat  atomic.Value
	lastRevLon  atomic.Value
	lastRevLang atomic.Value
}

func (f *fakeNominatim) Search(ctx context.Context, p nominatim.SearchParams) ([]nominatim.Place, error) {
	f.searchCalls.Add(1)
	f.lastSearch.Store(&p)
	if f.search == nil {
		return nil, nil
	}
	return f.search(ctx, p)
}

func (f *fakeNominatim) Reverse(ctx context.Context, lat, lon float64, langs []string) (*nominatim.Place, error) {
	f.revCalls.Add(1)
	f.lastRevLat.Store(lat)
	f.lastRevLon.Store(lon)
	f.lastRevLang.Store(langs)
	if f.reverse == nil {
		return nil, nominatim.ErrNotFound
	}
	return f.reverse(ctx, lat, lon, langs)
}

func (f *fakeNominatim) Health(ctx context.Context) error {
	if f.health == nil {
		return nil
	}
	return f.health(ctx)
}

type fakeRoute struct {
	route  func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error)
	health func(context.Context, geo.Point) error
	calls  atomic.Int32
	last   atomic.Pointer[osrm.RouteParams]
}

func (f *fakeRoute) Route(ctx context.Context, p osrm.RouteParams) (*osrm.RouteResponse, error) {
	f.calls.Add(1)
	f.last.Store(&p)
	if f.route == nil {
		return nil, osrm.ErrNoRoute
	}
	return f.route(ctx, p)
}

func (f *fakeRoute) Health(ctx context.Context, pt geo.Point) error {
	if f.health == nil {
		return nil
	}
	return f.health(ctx, pt)
}

type fakeTiles struct {
	tile  func(version string, z, x, y int) tiles.Result
	ready func(version string) error
}

func (f *fakeTiles) Tile(_ context.Context, version string, z, x, y int) tiles.Result {
	if f.tile == nil {
		return tiles.Result{Status: http.StatusNoContent}
	}
	return f.tile(version, z, x, y)
}

func (f *fakeTiles) Ready(_ context.Context, version string) error {
	if f.ready == nil {
		return nil
	}
	return f.ready(version)
}

// harness wires a Server to fakes.
type harness struct {
	t       *testing.T
	srv     *Server
	h       http.Handler
	photon  *fakePhoton
	nom     *fakeNominatim
	car     *fakeRoute
	foot    *fakeRoute
	tiles   *fakeTiles
	logs    *syncBuffer
	version atomic.Value
	cfg     *config.Gateway
	static  string
}

type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}

// newHarness builds a server with rate limits disabled and API key testKey.
// env overrides config variables; store may be nil (no cache).
func newHarness(t *testing.T, env map[string]string, store cache.Store) *harness {
	t.Helper()
	static := t.TempDir()
	vars := map[string]string{
		"API_KEYS":             testKey,
		"STATIC_DIR":           static,
		"RATE_LIMIT_IP_RPS":    "0",
		"RATE_LIMIT_KEY_RPS":   "0",
		"RATE_LIMIT_TILES_RPS": "0",
		"PUBLIC_BASE_URL":      "https://maps.example.com",
	}
	for k, v := range env {
		vars[k] = v
	}
	cfg, err := config.LoadGateway(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	hs := &harness{t: t, photon: &fakePhoton{}, nom: &fakeNominatim{}, car: &fakeRoute{}, foot: &fakeRoute{}, tiles: &fakeTiles{}, logs: &syncBuffer{}, cfg: cfg, static: static}
	hs.version.Store("20261014T000000Z")
	hs.srv = New(Deps{
		Cfg:       cfg,
		Log:       slog.New(slog.NewJSONHandler(hs.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Photon:    hs.photon,
		Nominatim: hs.nom,
		OSRM:      map[string]RouteAPI{"car": hs.car, "foot": hs.foot},
		Cache:     store,
		Tiles:     hs.tiles,
		Version:   func() string { return hs.version.Load().(string) },
	})
	hs.h = hs.srv.Handler()
	return hs
}

// do performs a request against the handler; headers are key, value pairs.
func (hs *harness) do(method, target string, headers ...string) *httptest.ResponseRecorder {
	hs.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "203.0.113.7:4321"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

// get performs an authenticated GET.
func (hs *harness) get(target string, headers ...string) *httptest.ResponseRecorder {
	hs.t.Helper()
	return hs.do("GET", target, append([]string{"X-API-Key", testKey}, headers...)...)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, rec.Body.String())
	}
	return v
}

// wantError asserts the unified error response.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errorDetail {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("error Content-Type = %q", ct)
	}
	body := decode[errorBody](t, rec)
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q with a message", body.Error, code)
	}
	return body.Error
}

func newMiniredisStore(t *testing.T) (*cache.Redis, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	r := cache.NewRedis(cache.Options{Addr: mr.Addr(), Backoff: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { r.Close() })
	return r, mr
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- sample upstream data (shaped like the recorded fixtures) -------------

func khanShatyrPhoton() photon.Feature {
	return photon.Feature{OSMType: "W", OSMID: 460703779, OSMKey: "shop", OSMValue: "mall", Type: "house",
		Name: "Хан Шатыр", HouseNumber: "37", Street: "проспект Туран", District: "район Нура", City: "Астана", Postcode: "Z19Y4X2",
		Lon: 71.4038607, Lat: 51.1325048}
}

func khanShatyrNominatim() nominatim.Place {
	return nominatim.Place{PlaceID: 124001, OSMType: "way", OSMID: 460703779, Lat: 51.1325, Lon: 71.4039, Category: "shop", Type: "mall",
		AddressType: "shop", Name: "Хан Шатыр", DisplayName: "Хан Шатыр, 37, проспект Туран, район Нура, Астана",
		Address: nominatim.Address{Road: "проспект Туран", HouseNumber: "37", CityDistrict: "район Нура", City: "Астана", Postcode: "Z19Y4X2"}}
}

func simpleRoute() *osrm.RouteResponse {
	return &osrm.RouteResponse{Code: "Ok",
		Routes: []osrm.Route{{Distance: 9045.2, Duration: 750.4, Geometry: osrm.RouteGeometry{Encoded: "_p~iF~ps|U_ulLnnqC"},
			Legs: []osrm.Leg{{Distance: 9045.2, Duration: 750.4, Summary: "проспект Туран"}}}},
		Waypoints: []osrm.Waypoint{{Name: "A", Location: [2]float64{71.4306, 51.1283}}, {Name: "B", Location: [2]float64{71.47, 51.18}}}}
}

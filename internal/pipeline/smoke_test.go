package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/geo"
	"map-service/internal/tiles"
)

// --- fakes for the smoke-test clients ---------------------------------------

type fakeNom struct {
	healthErrs []error // consumed one per Health call, then success
	alwaysDown error   // when set, Health never succeeds
	search     []nominatim.Place
	searchErr  error
	reverse    *nominatim.Place
	reverseErr error
	calls      int
}

func (f *fakeNom) Health(context.Context) error {
	f.calls++
	if f.alwaysDown != nil {
		return f.alwaysDown
	}
	if len(f.healthErrs) > 0 {
		e := f.healthErrs[0]
		f.healthErrs = f.healthErrs[1:]
		return e
	}
	return nil
}
func (f *fakeNom) Search(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
	return f.search, f.searchErr
}
func (f *fakeNom) Reverse(context.Context, float64, float64, []string) (*nominatim.Place, error) {
	return f.reverse, f.reverseErr
}

type fakeOSRMProbe struct {
	healthErr error
	routeErr  error
	route     *osrm.RouteResponse
}

func (f *fakeOSRMProbe) Health(context.Context, geo.Point) error { return f.healthErr }
func (f *fakeOSRMProbe) Route(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) {
	return f.route, f.routeErr
}

type fakePhotonProbe struct {
	healthErr error
	feats     []photon.Feature
	err       error
}

func (f *fakePhotonProbe) Health(context.Context) error { return f.healthErr }
func (f *fakePhotonProbe) Search(context.Context, photon.SearchParams) ([]photon.Feature, error) {
	return f.feats, f.err
}

type fakeTileProbe struct {
	readyErr error
	result   tiles.Result
	gotZ     int
	gotXY    [2]int
}

func (f *fakeTileProbe) Ready(context.Context, string) error { return f.readyErr }
func (f *fakeTileProbe) Tile(_ context.Context, _ string, z, x, y int) tiles.Result {
	f.gotZ, f.gotXY = z, [2]int{x, y}
	return f.result
}

// smokeEnv bundles the happy-path fakes; tests break one piece at a time.
type smokeEnv struct {
	nom   *fakeNom
	cars  map[string]*fakeOSRMProbe // by base URL
	ph    *fakePhotonProbe
	tile  *fakeTileProbe
	urls  []string // base URLs the probes were created for
	probe Probes
}

func healthyRoute() *osrm.RouteResponse {
	return &osrm.RouteResponse{Routes: []osrm.Route{{Distance: 1200, Duration: 150, Geometry: osrm.RouteGeometry{Encoded: "abc"}}}}
}

func newSmokeEnv() *smokeEnv {
	e := &smokeEnv{
		nom: &fakeNom{
			search:  []nominatim.Place{{Lat: 51.1325, Lon: 71.4039, DisplayName: "Хан Шатыр"}},
			reverse: &nominatim.Place{DisplayName: "проспект Туран, Астана"},
		},
		cars: map[string]*fakeOSRMProbe{},
		ph:   &fakePhotonProbe{feats: []photon.Feature{{Name: "Хан Шатыр"}}},
		tile: &fakeTileProbe{result: tiles.Result{Status: 200, Body: []byte("tile")}},
	}
	e.probe = Probes{
		Nominatim: func(u string) NominatimProbe { e.urls = append(e.urls, u); return e.nom },
		Photon:    func(u string) PhotonProbe { e.urls = append(e.urls, u); return e.ph },
		OSRM: func(u string) OSRMProbe {
			e.urls = append(e.urls, u)
			if e.cars[u] == nil {
				e.cars[u] = &fakeOSRMProbe{route: healthyRoute()}
			}
			return e.cars[u]
		},
		Tiles: func() (TileProbe, error) { return e.tile, nil },
	}
	return e
}

func smokeBuild(t *testing.T, e *smokeEnv, fr *fakeRunner) *Build {
	t.Helper()
	cfg := testConfig(t)
	cfg.Center = geo.Point{Lon: 71.4306, Lat: 51.1283}
	cfg.SmokeGeocodeQuery = "Хан Шатыр"
	cfg.SmokeGeocodeMinResults = 1
	b := newTestBuild(t, cfg, fr)
	b.Probes = e.probe
	b.CandidateTimeout = 50 * time.Millisecond
	return b
}

func TestSmokeHappyPath(t *testing.T) {
	e := newSmokeEnv()
	fr := &fakeRunner{}
	b := smokeBuild(t, e, fr)
	if err := b.Smoke(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Helper containers: one OSRM per profile (own profile data) and one Photon.
	started := fr.joined()
	for _, want := range []string{
		"run -d --no-deps --name mapsvc-cand-osrm-car osrm-tools osrm-routed --algorithm mld --port 5000 /data/city.osrm",
		"run -d --no-deps --name mapsvc-cand-osrm-foot osrm-tools osrm-routed --algorithm mld --port 5000 /data/city.osrm",
		"run -d --no-deps --name mapsvc-cand-photon photon-tools serve -data-dir /photon -listen-ip 0.0.0.0 -listen-port 2322",
	} {
		found := false
		for _, l := range started {
			found = found || strings.HasSuffix(l, want) || strings.Contains(l, want)
		}
		if !found {
			t.Errorf("missing command %q in:\n%s", want, strings.Join(started, "\n"))
		}
	}
	for _, c := range fr.runs {
		line := strings.Join(c.Args, " ")
		for _, p := range []string{"car", "foot"} {
			if strings.Contains(line, "--name mapsvc-cand-osrm-"+p+" ") && c.Env["OSRM_PROFILE"] != p {
				t.Errorf("candidate for %s must mount that profile's data (OSRM_PROFILE=%q)", p, c.Env["OSRM_PROFILE"])
			}
		}
	}
	// The probes target the candidates by container name on the internal network.
	for _, want := range []string{
		"http://mapsvc-nominatim-candidate:8080", "http://mapsvc-cand-osrm-car:5000",
		"http://mapsvc-cand-osrm-foot:5000", "http://mapsvc-cand-photon:2322",
	} {
		ok := false
		for _, u := range e.urls {
			ok = ok || u == want
		}
		if !ok {
			t.Errorf("no probe for %s (got %v)", want, e.urls)
		}
	}
	// The z14 tile of the city centre was requested.
	if e.tile.gotZ != 14 || e.tile.gotXY != [2]int{11442, 5475} {
		t.Errorf("tile probe: z=%d xy=%v", e.tile.gotZ, e.tile.gotXY)
	}
	// All helpers are removed afterwards.
	var removed []string
	for _, c := range fr.outs {
		if len(c.Args) >= 3 && c.Args[0] == "rm" {
			removed = append(removed, c.Args[len(c.Args)-1])
		}
	}
	for _, name := range []string{"mapsvc-cand-osrm-car", "mapsvc-cand-osrm-foot", "mapsvc-cand-photon"} {
		found := false
		for _, r := range removed {
			found = found || r == name
		}
		if !found {
			t.Errorf("helper %s not removed (removed: %v)", name, removed)
		}
	}
	// The candidate Nominatim is NOT touched by Smoke: it becomes the live one.
	for _, r := range removed {
		if r == "mapsvc-nominatim-candidate" {
			t.Error("Smoke must not remove the candidate Nominatim")
		}
	}
}

func TestSmokeWaitsForSlowCandidates(t *testing.T) {
	e := newSmokeEnv()
	e.nom.healthErrs = []error{errors.New("starting"), errors.New("starting")}
	b := smokeBuild(t, e, &fakeRunner{})
	b.CandidateTimeout = 2 * time.Second
	if err := b.Smoke(context.Background()); err != nil {
		t.Fatalf("a candidate that needs a few polls must be waited for: %v", err)
	}
	if e.nom.calls != 3 {
		t.Errorf("health polled %d times, want 3", e.nom.calls)
	}
}

func TestSmokeFailures(t *testing.T) {
	outside := nominatim.Place{Lat: 43.25, Lon: 76.95, DisplayName: "Алматы"}
	cases := []struct {
		name   string
		break_ func(*smokeEnv)
		want   string
	}{
		{"tile archive unreadable", func(e *smokeEnv) { e.tile.readyErr = errors.New("archive not found") }, "tiles"},
		{"empty centre tile", func(e *smokeEnv) { e.tile.result = tiles.Result{Status: 204} }, "tiles"},
		{"nominatim never healthy", func(e *smokeEnv) { e.nom.alwaysDown = errors.New("connection refused") }, "nominatim did not become ready"},
		{"geocode fails", func(e *smokeEnv) { e.nom.searchErr = errors.New("boom") }, "geocode"},
		{"geocode finds nothing", func(e *smokeEnv) { e.nom.search = nil }, "returned 0 results"},
		{"geocode only outside the city", func(e *smokeEnv) { e.nom.search = []nominatim.Place{outside} }, "returned 0 results"},
		{"reverse not found", func(e *smokeEnv) { e.nom.reverseErr = nominatim.ErrNotFound }, "reverse"},
		{"reverse empty", func(e *smokeEnv) { e.nom.reverse = &nominatim.Place{} }, "empty address"},
		{"route fails for car", func(e *smokeEnv) {
			e.cars["http://mapsvc-cand-osrm-car:5000"] = &fakeOSRMProbe{routeErr: osrm.ErrNoRoute}
		}, "route (car)"},
		{"route fails for foot", func(e *smokeEnv) {
			e.cars["http://mapsvc-cand-osrm-foot:5000"] = &fakeOSRMProbe{routeErr: osrm.ErrNoRoute}
		}, "route (foot)"},
		{"degenerate route", func(e *smokeEnv) {
			e.cars["http://mapsvc-cand-osrm-car:5000"] = &fakeOSRMProbe{route: &osrm.RouteResponse{Routes: []osrm.Route{{Distance: 0, Duration: 0}}}}
		}, "degenerate"},
		{"photon unhealthy", func(e *smokeEnv) { e.ph.healthErr = errors.New("down") }, "photon did not become ready"},
		{"photon finds nothing", func(e *smokeEnv) { e.ph.feats = nil }, "autocomplete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSmokeEnv()
			c.break_(e)
			fr := &fakeRunner{}
			b := smokeBuild(t, e, fr)
			err := b.Smoke(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), "smoke:") {
				t.Errorf("error should be prefixed with the check: %v", err)
			}
			// Helpers are cleaned up even when a check fails.
			removed := 0
			for _, o := range fr.outs {
				if len(o.Args) > 0 && o.Args[0] == "rm" {
					removed++
				}
			}
			if removed != 3 {
				t.Errorf("helper containers removed = %d, want 3", removed)
			}
		})
	}
}

func TestSmokeStartFailureStillCleansUp(t *testing.T) {
	e := newSmokeEnv()
	fr := &fakeRunner{onRun: func(c Cmd) error {
		if strings.Contains(strings.Join(c.Args, " "), "--name mapsvc-cand-osrm-foot") {
			return errors.New("docker: no space left on device")
		}
		return nil
	}}
	b := smokeBuild(t, e, fr)
	err := b.Smoke(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v", err)
	}
	removed := map[string]bool{}
	for _, o := range fr.outs {
		if len(o.Args) > 0 && o.Args[0] == "rm" {
			removed[o.Args[len(o.Args)-1]] = true
		}
	}
	if !removed["mapsvc-cand-osrm-car"] || !removed["mapsvc-cand-osrm-foot"] {
		t.Errorf("already started helpers must be removed: %v", removed)
	}
}

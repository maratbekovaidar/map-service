package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
)

// --------------------------------------------------------------- validation

func TestValidationErrors(t *testing.T) {
	hs := newHarness(t, nil, nil)
	long := url.QueryEscape(strings.Repeat("я", 201))
	cases := []struct {
		name, target, mention string
	}{
		{"autocomplete without q", "/v1/autocomplete", "q"},
		{"autocomplete blank q", "/v1/autocomplete?q=%20%20", "q"},
		{"autocomplete q too long", "/v1/autocomplete?q=" + long, "q"},
		{"autocomplete control char", "/v1/autocomplete?q=a%00b", "control"},
		{"autocomplete limit zero", "/v1/autocomplete?q=a&limit=0", "limit"},
		{"autocomplete limit over max", "/v1/autocomplete?q=a&limit=21", "limit"},
		{"autocomplete limit text", "/v1/autocomplete?q=a&limit=many", "limit"},
		{"autocomplete bad lang", "/v1/autocomplete?q=a&lang=de", "lang"},
		{"autocomplete lat without lon", "/v1/autocomplete?q=a&lat=51.1", "together"},
		{"autocomplete lat out of range", "/v1/autocomplete?q=a&lat=91&lon=71", "lat"},
		{"autocomplete NaN lon", "/v1/autocomplete?q=a&lat=51&lon=NaN", "lon"},
		{"geocode without q", "/v1/geocode", "q"},
		{"geocode bad lang", "/v1/geocode?q=a&lang=xx", "lang"},
		{"reverse without lat", "/v1/reverse?lon=71.43", "lat"},
		{"reverse without lon", "/v1/reverse?lat=51.12", "lon"},
		{"reverse text lat", "/v1/reverse?lat=abc&lon=71.43", "lat"},
		{"reverse outside city", "/v1/reverse?lat=43.25&lon=76.95", "outside"},
		{"route without profile", "/v1/route?points=71.4,51.1;71.5,51.2", "profile"},
		{"route unknown profile", "/v1/route?profile=boat&points=71.4,51.1;71.5,51.2", "profile"},
		{"route without points", "/v1/route?profile=car", "points"},
		{"route one point", "/v1/route?profile=car&points=71.4,51.1", "at least two"},
		{"route garbage point", "/v1/route?profile=car&points=71.4,51.1;oops", "points[1]"},
		{"route point out of range", "/v1/route?profile=car&points=71.4,51.1;71.5,95", "points[1]"},
		{"route point outside city", "/v1/route?profile=car&points=71.4,51.1;76.95,43.25", "outside"},
		{"route too many points", "/v1/route?profile=car&points=" + strings.TrimSuffix(strings.Repeat("71.4,51.1;", 26), ";"), "at most 25"},
		{"route bad alternatives", "/v1/route?profile=car&points=71.4,51.1;71.5,51.2&alternatives=maybe", "alternatives"},
		{"route bad steps", "/v1/route?profile=car&points=71.4,51.1;71.5,51.2&steps=2", "steps"},
		{"route bad geometry", "/v1/route?profile=car&points=71.4,51.1;71.5,51.2&geometry=wkt", "geometry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := hs.get(c.target)
			e := wantError(t, rec, http.StatusBadRequest, CodeInvalidArgument)
			if !strings.Contains(e.Message, c.mention) {
				t.Errorf("message %q should mention %q", e.Message, c.mention)
			}
		})
	}
	// Nothing invalid may reach an upstream.
	if hs.photon.calls.Load()+hs.nom.searchCalls.Load()+hs.nom.revCalls.Load()+hs.car.calls.Load() != 0 {
		t.Error("an upstream was called for an invalid request")
	}
}

func TestLimitDefaultsAndBoundary(t *testing.T) {
	hs := newHarness(t, map[string]string{"MAX_RESULTS_LIMIT": "10"}, nil)
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		return []photon.Feature{khanShatyrPhoton()}, nil
	}
	if rec := hs.get("/v1/autocomplete?q=x"); rec.Code != 200 || hs.photon.last.Load().Limit != 5 {
		t.Errorf("default limit: code=%d limit=%d", rec.Code, hs.photon.last.Load().Limit)
	}
	if rec := hs.get("/v1/autocomplete?q=x&limit=10"); rec.Code != 200 || hs.photon.last.Load().Limit != 10 {
		t.Errorf("limit == max must pass: code=%d", rec.Code)
	}
	wantError(t, hs.get("/v1/autocomplete?q=x&limit=11"), 400, CodeInvalidArgument)
}

// ------------------------------------------------------------- autocomplete

func TestAutocompleteFromPhoton(t *testing.T) {
	hs := newHarness(t, nil, nil)
	outside := khanShatyrPhoton()
	outside.Name, outside.Lon, outside.Lat = "Almaty", 76.95, 43.25
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		return []photon.Feature{khanShatyrPhoton(), outside}, nil
	}
	rec := hs.get("/v1/autocomplete?q=%D1%85%D0%B0%D0%BD&lat=51.13&lon=71.43&limit=3&lang=KK")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decode[PlacesResponse](t, rec)
	if len(got.Places) != 1 {
		t.Fatalf("a place outside CITY_BBOX must be dropped; got %+v", got.Places)
	}
	want := Place{ID: "way/460703779", Name: "Хан Шатыр", Type: "mall", Source: "photon", Lat: 51.1325048, Lon: 71.4038607,
		Address: Address{Street: "проспект Туран", HouseNumber: "37", District: "район Нура", City: "Астана", Postcode: "Z19Y4X2"}}
	if got.Places[0] != want {
		t.Errorf("place:\n got  %+v\n want %+v", got.Places[0], want)
	}

	// Parameters reach Photon: language normalised, bias, bbox restriction, limit.
	p := hs.photon.last.Load()
	if p.Query != "хан" || p.Lang != "kk" || p.Limit != 3 || p.Bias == nil || p.Bias.Lat != 51.13 || p.Bias.Lon != 71.43 {
		t.Errorf("photon params: %+v", p)
	}
	if p.BBox == nil || p.BBox.String() != "71.04,50.74,71.96,51.46" {
		t.Errorf("photon must be restricted to the city bbox: %+v", p.BBox)
	}
	if hs.nom.searchCalls.Load() != 0 {
		t.Error("no fallback expected when Photon answered")
	}
}

func TestAutocompleteUnnamedAddressGetsReadableName(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		return []photon.Feature{{OSMType: "W", OSMID: 1, OSMKey: "place", OSMValue: "house", Type: "house",
			HouseNumber: "37", Street: "проспект Туран", City: "Астана", Lon: 71.4, Lat: 51.1}}, nil
	}
	got := decode[PlacesResponse](t, hs.get("/v1/autocomplete?q=x"))
	if p := got.Places[0]; p.Name != "проспект Туран, 37" || p.Type != "house" {
		t.Errorf("unnamed address: %+v", p)
	}
}

func TestAutocompleteFallsBackToNominatim(t *testing.T) {
	scenarios := map[string]func(*harness){
		"photon error": func(hs *harness) {
			hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
				return nil, errors.New("connection refused")
			}
		},
		"photon empty": func(hs *harness) {
			hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) { return nil, nil }
		},
		"photon only outside the city": func(hs *harness) {
			hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
				f := khanShatyrPhoton()
				f.Lon, f.Lat = 76.95, 43.25
				return []photon.Feature{f}, nil
			}
		},
	}
	for name, setup := range scenarios {
		t.Run(name, func(t *testing.T) {
			hs := newHarness(t, nil, nil)
			setup(hs)
			hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
				return []nominatim.Place{khanShatyrNominatim()}, nil
			}
			rec := hs.get("/v1/autocomplete?q=%D1%85%D0%B0%D0%BD&lang=ru&limit=4")
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			got := decode[PlacesResponse](t, rec)
			if len(got.Places) != 1 || got.Places[0].Source != "nominatim" || got.Places[0].ID != "way/460703779" {
				t.Fatalf("expected the nominatim result: %+v", got.Places)
			}
			ns := hs.nom.lastSearch.Load()
			if ns.Limit != 4 || ns.Bounds == nil || ns.Languages[0] != "ru" || len(ns.Languages) != 3 {
				t.Errorf("nominatim params: %+v", ns)
			}
		})
	}
}

func TestAutocompleteBothBackendsFail(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) { return nil, errors.New("down") }
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, errors.New("down too")
	}
	e := wantError(t, hs.get("/v1/autocomplete?q=x"), http.StatusBadGateway, CodeUpstreamUnavailable)
	if strings.Contains(e.Message, "down") {
		t.Errorf("internal error text must not leak to clients: %q", e.Message)
	}
	if !strings.Contains(hs.logs.String(), "down too") {
		t.Error("the cause must be logged server-side")
	}
}

func TestAutocompleteTimeouts(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.photon.search = func(context.Context, photon.SearchParams) ([]photon.Feature, error) {
		return nil, context.DeadlineExceeded
	}
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, context.DeadlineExceeded
	}
	wantError(t, hs.get("/v1/autocomplete?q=x"), http.StatusGatewayTimeout, CodeUpstreamTimeout)
}

func TestAutocompleteEmptyPhotonAndFailingFallbackIsStill200(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, errors.New("down")
	}
	rec := hs.get("/v1/autocomplete?q=x")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"places":[]`) {
		t.Errorf("places must serialise as [] not null: %s", rec.Body)
	}
}

// ------------------------------------------------------------------ geocode

func TestGeocode(t *testing.T) {
	hs := newHarness(t, nil, nil)
	outside := khanShatyrNominatim()
	outside.PlaceID, outside.Lat, outside.Lon = 2, 43.25, 76.95
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return []nominatim.Place{khanShatyrNominatim(), outside}, nil
	}
	rec := hs.get("/v1/geocode?q=%D0%A5%D0%B0%D0%BD+%D0%A8%D0%B0%D1%82%D1%8B%D1%80&lang=kk")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decode[PlacesResponse](t, rec)
	if len(got.Places) != 1 {
		t.Fatalf("outside-bbox result must be filtered: %+v", got.Places)
	}
	p := got.Places[0]
	if p.Source != "nominatim" || p.ID != "way/460703779" || p.Type != "mall" || p.Address.District != "район Нура" ||
		p.Address.Street != "проспект Туран" || p.Address.HouseNumber != "37" || p.Address.City != "Астана" {
		t.Errorf("place: %+v", p)
	}
	ns := hs.nom.lastSearch.Load()
	if ns.Query != "Хан Шатыр" || ns.Limit != 5 || ns.Languages[0] != "kk" || ns.Bounds == nil {
		t.Errorf("nominatim params: %+v", ns)
	}
	if hs.photon.calls.Load() != 0 {
		t.Error("geocode must not use Photon")
	}
}

func TestGeocodeMappingFallbacks(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return []nominatim.Place{
			// building without a name: name from the address; generic type -> addresstype
			{PlaceID: 7, OSMType: "node", OSMID: 99, Lat: 51.1, Lon: 71.4, Type: "yes", AddressType: "building", Name: "",
				DisplayName: "37, проспект Туран, Астана", Address: nominatim.Address{Road: "проспект Туран", HouseNumber: "37", Suburb: "Центр", Town: "Город"}},
			// nothing but a display name; no OSM identity
			{PlaceID: 8, Lat: 51.2, Lon: 71.5, Type: "", AddressType: "", Category: "place", DisplayName: "Какое-то место, Астана"},
		}, nil
	}
	got := decode[PlacesResponse](t, hs.get("/v1/geocode?q=x")).Places
	if len(got) != 2 {
		t.Fatal(got)
	}
	if got[0].Name != "проспект Туран, 37" || got[0].Type != "building" || got[0].Address.District != "Центр" || got[0].Address.City != "Город" || got[0].ID != "node/99" {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Name != "Какое-то место" || got[1].ID != "nominatim/8" || got[1].Type != "place" {
		t.Errorf("second: %+v", got[1])
	}
}

func TestGeocodeUpstreamErrors(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, errors.New("boom")
	}
	wantError(t, hs.get("/v1/geocode?q=x"), http.StatusBadGateway, CodeUpstreamUnavailable)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		return nil, context.DeadlineExceeded
	}
	wantError(t, hs.get("/v1/geocode?q=x"), http.StatusGatewayTimeout, CodeUpstreamTimeout)
}

// ------------------------------------------------------------------ reverse

func TestReverse(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.reverse = func(context.Context, float64, float64, []string) (*nominatim.Place, error) {
		p := khanShatyrNominatim()
		return &p, nil
	}
	rec := hs.get("/v1/reverse?lat=51.13250123&lon=71.40390987&lang=en")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if p := decode[Place](t, rec); p.Name != "Хан Шатыр" || p.Source != "nominatim" {
		t.Errorf("place: %+v", p)
	}
	// Upstream sees the rounded coordinates and the language priority.
	if lat := hs.nom.lastRevLat.Load().(float64); lat != 51.1325 {
		t.Errorf("lat sent upstream = %v, want rounded 51.1325", lat)
	}
	if lon := hs.nom.lastRevLon.Load().(float64); lon != 71.40391 { // 71.40390987 rounded to 5 decimals
		t.Errorf("lon sent upstream = %v, want rounded 71.40391", lon)
	}
	if langs := hs.nom.lastRevLang.Load().([]string); langs[0] != "en" || len(langs) != 3 {
		t.Errorf("languages = %v", langs)
	}
}

func TestReverseNotFound(t *testing.T) {
	hs := newHarness(t, nil, nil)
	// default fake returns nominatim.ErrNotFound
	wantError(t, hs.get("/v1/reverse?lat=51.1&lon=71.4"), http.StatusNotFound, CodeNotFound)

	// An object whose own coordinates lie outside the service area is not returned either.
	hs.nom.reverse = func(context.Context, float64, float64, []string) (*nominatim.Place, error) {
		p := khanShatyrNominatim()
		p.Lat, p.Lon = 43.25, 76.95
		return &p, nil
	}
	wantError(t, hs.get("/v1/reverse?lat=51.1&lon=71.4"), http.StatusNotFound, CodeNotFound)
}

func TestReverseUpstreamErrors(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.reverse = func(context.Context, float64, float64, []string) (*nominatim.Place, error) {
		return nil, errors.New("boom")
	}
	wantError(t, hs.get("/v1/reverse?lat=51.1&lon=71.4"), http.StatusBadGateway, CodeUpstreamUnavailable)
	hs.nom.reverse = func(context.Context, float64, float64, []string) (*nominatim.Place, error) {
		return nil, context.DeadlineExceeded
	}
	wantError(t, hs.get("/v1/reverse?lat=51.1&lon=71.4"), http.StatusGatewayTimeout, CodeUpstreamTimeout)
}

// -------------------------------------------------------------------- route

func TestRouteMapping(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.car.route = func(_ context.Context, p osrm.RouteParams) (*osrm.RouteResponse, error) {
		res := simpleRoute()
		if p.Geometry == osrm.GeoJSON {
			res.Routes[0].Geometry = osrm.RouteGeometry{Line: osrm.LineString{{71.4306, 51.1283}, {71.47, 51.18}}}
		}
		if p.Steps {
			res.Routes[0].Legs[0].Steps = []osrm.Step{{Distance: 120.5, Duration: 14.2, Name: "проспект Туран",
				Maneuver: osrm.Maneuver{Type: "turn", Modifier: "left", Location: [2]float64{71.43, 51.12}},
				Geometry: osrm.RouteGeometry{Encoded: "abc", Line: osrm.LineString{{71.43, 51.12}, {71.44, 51.13}}}}}
		}
		return res, nil
	}
	pts := "points=71.4306,51.1283;71.47,51.18"

	// polyline, no steps
	rec := hs.get("/v1/route?profile=car&" + pts)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var raw struct {
		Routes []struct {
			DistanceM float64          `json:"distance_m"`
			DurationS float64          `json:"duration_s"`
			Geometry  json.RawMessage  `json:"geometry"`
			Legs      []map[string]any `json:"legs"`
		} `json:"routes"`
		Waypoints []Waypoint `json:"waypoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	r := raw.Routes[0]
	if r.DistanceM != 9045.2 || r.DurationS != 750.4 || string(r.Geometry) != `"_p~iF~ps|U_ulLnnqC"` {
		t.Errorf("route: %+v geometry=%s", r, r.Geometry)
	}
	if _, has := r.Legs[0]["steps"]; has {
		t.Error("steps must be omitted unless steps=true")
	}
	if len(raw.Waypoints) != 2 || raw.Waypoints[0].Lon != 71.4306 || raw.Waypoints[0].Lat != 51.1283 {
		t.Errorf("waypoints (lon/lat order): %+v", raw.Waypoints)
	}
	if p := hs.car.last.Load(); p.Alternatives || p.Steps || p.Geometry != osrm.Polyline || len(p.Points) != 2 {
		t.Errorf("osrm params: %+v", p)
	}

	// geojson + steps + alternatives
	rec = hs.get("/v1/route?profile=car&" + pts + "&geometry=geojson&steps=true&alternatives=true")
	var gj struct {
		Routes []struct {
			Geometry struct {
				Type        string       `json:"type"`
				Coordinates [][2]float64 `json:"coordinates"`
			} `json:"geometry"`
			Legs []struct {
				Steps []struct {
					Name     string   `json:"name"`
					Maneuver Maneuver `json:"maneuver"`
					Geometry struct {
						Type string `json:"type"`
					} `json:"geometry"`
				} `json:"steps"`
			} `json:"legs"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gj); err != nil {
		t.Fatal(err)
	}
	g := gj.Routes[0].Geometry
	if g.Type != "LineString" || len(g.Coordinates) != 2 || g.Coordinates[0][0] != 71.4306 {
		t.Errorf("geojson geometry: %+v", g)
	}
	st := gj.Routes[0].Legs[0].Steps[0]
	if st.Name != "проспект Туран" || st.Maneuver.Type != "turn" || st.Maneuver.Modifier != "left" || st.Maneuver.Lon != 71.43 || st.Maneuver.Lat != 51.12 || st.Geometry.Type != "LineString" {
		t.Errorf("step: %+v", st)
	}
	if p := hs.car.last.Load(); !p.Alternatives || !p.Steps || p.Geometry != osrm.GeoJSON {
		t.Errorf("osrm params: %+v", p)
	}
}

func TestRouteSelectsProfile(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.foot.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) { return simpleRoute(), nil }
	if rec := hs.get("/v1/route?profile=foot&points=71.4,51.1;71.5,51.2"); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if hs.foot.calls.Load() != 1 || hs.car.calls.Load() != 0 {
		t.Errorf("foot=%d car=%d: the profile's own OSRM must be used", hs.foot.calls.Load(), hs.car.calls.Load())
	}
}

func TestRouteErrors(t *testing.T) {
	hs := newHarness(t, nil, nil)
	target := "/v1/route?profile=car&points=71.4,51.1;71.5,51.2"

	wantError(t, hs.get(target), http.StatusNotFound, CodeNoRoute) // fake default is ErrNoRoute

	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) {
		return nil, &osrm.APIError{Status: 400, Code: "InvalidQuery", Message: "internal detail"}
	}
	e := wantError(t, hs.get(target), http.StatusBadGateway, CodeUpstreamUnavailable)
	if strings.Contains(e.Message, "internal detail") {
		t.Error("OSRM's message must not leak")
	}

	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) {
		return nil, context.DeadlineExceeded
	}
	wantError(t, hs.get(target), http.StatusGatewayTimeout, CodeUpstreamTimeout)

	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) {
		return nil, errors.New("connection refused")
	}
	wantError(t, hs.get(target), http.StatusBadGateway, CodeUpstreamUnavailable)
}

// points=lon,lat;lon,lat is the documented format: the ';' must work both raw
// (curl, browsers) and percent-encoded (most HTTP libraries).
func TestRoutePointsSemicolonRawAndEncoded(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.car.route = func(context.Context, osrm.RouteParams) (*osrm.RouteResponse, error) { return simpleRoute(), nil }
	for _, target := range []string{
		"/v1/route?profile=car&points=71.4306,51.1283;71.47,51.18",
		"/v1/route?profile=car&points=71.4306%2C51.1283%3B71.47%2C51.18",
		"/v1/route?points=71.4306,51.1283;71.47,51.18&profile=car&steps=false",
	} {
		if rec := hs.get(target); rec.Code != 200 {
			t.Errorf("%s: status %d: %s", target, rec.Code, rec.Body)
		}
		if p := hs.car.last.Load(); len(p.Points) != 2 || p.Points[1].Lon != 71.47 || p.Points[1].Lat != 51.18 {
			t.Errorf("%s: points = %+v", target, p.Points)
		}
	}
}

func TestMalformedQueryEscapeIsRejected(t *testing.T) {
	hs := newHarness(t, nil, nil)
	e := wantError(t, hs.get("/v1/geocode?q=%zz"), http.StatusBadRequest, CodeInvalidArgument)
	if !strings.Contains(e.Message, "percent-escape") {
		t.Errorf("message: %q", e.Message)
	}
}

func TestJSONIsNotHTMLEscaped(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.nom.search = func(context.Context, nominatim.SearchParams) ([]nominatim.Place, error) {
		p := khanShatyrNominatim()
		p.Name = "Tom & Jerry <bar>"
		return []nominatim.Place{p}, nil
	}
	if body := hs.get("/v1/geocode?q=x").Body.String(); !strings.Contains(body, `"name":"Tom & Jerry <bar>"`) {
		t.Errorf("names must not be HTML-escaped: %s", body)
	}
	if body := hs.get("/tiles/15/0/0.mvt").Body.String(); !strings.Contains(body, "0 <= z <= 14") || strings.Contains(body, `\u003c`) {
		t.Errorf("error messages must not be HTML-escaped: %s", body)
	}
}

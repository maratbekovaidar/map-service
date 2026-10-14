//go:build integration

// Package integration tests a RUNNING stack through its public HTTP API
// (make up && make test-integration). The expectations are about the city the
// stack serves; the defaults match Astana and can be changed through the IT_*
// variables for another city.
//
//	GATEWAY_URL         base URL of the stack (default http://localhost)
//	API_KEY             an accepted X-API-Key (required)
//	CITY_CENTER         "lon,lat" of the city centre (default Astana)
//	ROUTING_PROFILES    profiles to test (default car,foot)
//	IT_GEOCODE_QUERY    a place that exists in the city (default "Хан Шатыр")
//	IT_GEOCODE_POINT    "lat,lon" of that place (default 51.1325,71.4039)
//	IT_REVERSE_CITY     city name Nominatim returns for the centre (default "Астана")
package integration

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"map-service/internal/geo"
)

var (
	baseURL  = env("GATEWAY_URL", "http://localhost")
	apiKey   = os.Getenv("API_KEY")
	profiles = strings.Split(env("ROUTING_PROFILES", "car,foot"), ",")
	// DisableCompression: the default transport asks for gzip, silently
	// decodes the answer and drops Content-Encoding - which would hide exactly
	// what the tile tests must see (gzip tiles on the wire). Requests send
	// "Accept-Encoding: gzip" themselves, like every real client; Caddy would
	// otherwise hand tiles to a client that does not accept gzip decompressed.
	client = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableCompression: true}}
)

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func mustPoint(s string) geo.Point {
	p, err := geo.ParsePoint(s)
	if err != nil {
		panic(err)
	}
	return p
}

func center() geo.Point { return mustPoint(env("CITY_CENTER", "71.4306,51.1283")) }

func TestMain(m *testing.M) {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "API_KEY is required (an entry of API_KEYS from .env)")
		os.Exit(2)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		resp, err := client.Get(baseURL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "the stack at %s is not ready (GET /readyz): %v\n", baseURL, err)
			os.Exit(2)
		}
		time.Sleep(3 * time.Second)
	}
	os.Exit(m.Run())
}

// get performs a request; withKey adds X-API-Key.
func get(t *testing.T, path string, withKey bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	if withKey {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Caddy compresses JSON/text for clients that accept gzip. Decode it here,
	// but leave vector tiles alone: their gzip is part of what is under test.
	if resp.Header.Get("Content-Encoding") == "gzip" && !strings.HasSuffix(path, ".mvt") {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				body = plain
			}
		}
	}
	return resp, body
}

func getJSON(t *testing.T, path string, v any) *http.Response {
	t.Helper()
	resp, body := get(t, path, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("GET %s: invalid JSON: %v\n%s", path, err, body)
	}
	return resp
}

// distanceM is the great-circle distance in metres.
func distanceM(a, b geo.Point) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (b.Lat-a.Lat)*rad, (b.Lon-a.Lon)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

type place struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address struct {
		Street      string `json:"street"`
		HouseNumber string `json:"house_number"`
		District    string `json:"district"`
		City        string `json:"city"`
		Postcode    string `json:"postcode"`
	} `json:"address"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Type   string  `json:"type"`
	Source string  `json:"source"`
}

func TestReadyz(t *testing.T) {
	var r struct {
		Status      string `json:"status"`
		DataVersion string `json:"data_version"`
		Checks      map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	getJSON(t, "/readyz", &r)
	if r.Status != "ok" || r.DataVersion == "" || r.DataVersion == "none" {
		t.Fatalf("readyz: %+v", r)
	}
	want := []string{"photon", "nominatim", "tiles"}
	for _, p := range profiles {
		want = append(want, "osrm-"+p)
	}
	for _, name := range want {
		if r.Checks[name].Status != "ok" {
			t.Errorf("check %s = %q, want ok", name, r.Checks[name].Status)
		}
	}
}

func TestGeocodeKnownPlace(t *testing.T) {
	query := env("IT_GEOCODE_QUERY", "Хан Шатыр")
	want := mustPoint(func() string { // IT_GEOCODE_POINT is "lat,lon"; ParsePoint wants "lon,lat"
		parts := strings.Split(env("IT_GEOCODE_POINT", "51.1325,71.4039"), ",")
		return strings.TrimSpace(parts[1]) + "," + strings.TrimSpace(parts[0])
	}())
	var res struct{ Places []place }
	getJSON(t, "/v1/geocode?limit=5&q="+url.QueryEscape(query), &res)
	if len(res.Places) == 0 {
		t.Fatalf("geocode %q returned nothing", query)
	}
	best := res.Places[0]
	if d := distanceM(want, geo.Point{Lon: best.Lon, Lat: best.Lat}); d > 1500 {
		t.Errorf("first result %q is %.0f m from the expected place", best.Name, d)
	}
	if best.Source != "nominatim" || best.ID == "" || best.Type == "" || best.Address.City == "" {
		t.Errorf("incomplete place: %+v", best)
	}
	for _, p := range res.Places {
		if p.Lat < 50.74 || p.Lat > 51.46 {
			t.Errorf("result outside the service area: %+v", p)
		}
	}
}

func TestGeocodeLanguages(t *testing.T) {
	// Languages from LANGUAGES must be accepted, others rejected.
	for _, l := range []string{"ru", "kk", "en"} {
		var res struct{ Places []place }
		getJSON(t, "/v1/geocode?limit=1&lang="+l+"&q="+url.QueryEscape(env("IT_GEOCODE_QUERY", "Хан Шатыр")), &res)
		if len(res.Places) == 0 {
			t.Errorf("lang=%s: no results", l)
		}
	}
	resp, _ := get(t, "/v1/geocode?lang=de&q=x", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unsupported language: HTTP %d, want 400", resp.StatusCode)
	}
}

func TestReverseKnownPoint(t *testing.T) {
	c := center()
	var p place
	getJSON(t, fmt.Sprintf("/v1/reverse?lat=%.5f&lon=%.5f", c.Lat, c.Lon), &p)
	if p.Address.City != env("IT_REVERSE_CITY", "Астана") {
		t.Errorf("city = %q, want %q (%+v)", p.Address.City, env("IT_REVERSE_CITY", "Астана"), p)
	}
	if d := distanceM(c, geo.Point{Lon: p.Lon, Lat: p.Lat}); d > 500 {
		t.Errorf("the object is %.0f m from the requested point", d)
	}
	if p.Source != "nominatim" || p.ID == "" {
		t.Errorf("place: %+v", p)
	}
}

func TestAutocomplete(t *testing.T) {
	prefix := env("IT_AUTOCOMPLETE_PREFIX", "хан ш")
	var res struct{ Places []place }
	c := center()
	getJSON(t, fmt.Sprintf("/v1/autocomplete?limit=5&lat=%.5f&lon=%.5f&q=%s", c.Lat, c.Lon, url.QueryEscape(prefix)), &res)
	if len(res.Places) == 0 {
		t.Fatalf("autocomplete %q returned nothing", prefix)
	}
	found := false
	for _, p := range res.Places {
		found = found || strings.Contains(strings.ToLower(p.Name), "хан")
		if p.Source != "photon" && p.Source != "nominatim" {
			t.Errorf("unexpected source %q", p.Source)
		}
	}
	if !found {
		t.Errorf("no suggestion matches the prefix: %+v", res.Places)
	}
}

type route struct {
	DistanceM float64         `json:"distance_m"`
	DurationS float64         `json:"duration_s"`
	Geometry  json.RawMessage `json:"geometry"`
	Legs      []struct {
		DistanceM float64 `json:"distance_m"`
		Steps     []struct {
			Name     string `json:"name"`
			Maneuver struct {
				Type string `json:"type"`
			} `json:"maneuver"`
		} `json:"steps"`
	} `json:"legs"`
}

func TestRouteEveryProfile(t *testing.T) {
	c := center()
	// About 1.5 km north-east of the centre, inside the city.
	end := geo.Point{Lon: c.Lon + 0.02, Lat: c.Lat + 0.012}
	for _, profile := range profiles {
		t.Run(profile, func(t *testing.T) {
			var res struct {
				Routes    []route `json:"routes"`
				Waypoints []struct{ Lat, Lon float64 }
			}
			path := fmt.Sprintf("/v1/route?profile=%s&points=%.5f,%.5f;%.5f,%.5f", profile, c.Lon, c.Lat, end.Lon, end.Lat)
			getJSON(t, path, &res)
			if len(res.Routes) != 1 || len(res.Waypoints) != 2 {
				t.Fatalf("routes=%d waypoints=%d", len(res.Routes), len(res.Waypoints))
			}
			r := res.Routes[0]
			straight := distanceM(c, end)
			if r.DistanceM < straight*0.9 || r.DistanceM > straight*4 {
				t.Errorf("distance %.0f m is implausible for %.0f m in a straight line", r.DistanceM, straight)
			}
			if r.DurationS <= 0 || len(r.Legs) != 1 {
				t.Errorf("duration=%v legs=%d", r.DurationS, len(r.Legs))
			}
			// The default geometry is an encoded polyline of the whole route.
			var enc string
			if err := json.Unmarshal(r.Geometry, &enc); err != nil || enc == "" {
				t.Fatalf("geometry is not a polyline string: %s", r.Geometry)
			}
			pts, err := geo.DecodePolyline(enc, 5)
			if err != nil || len(pts) < 2 {
				t.Fatalf("polyline: %v, %d points", err, len(pts))
			}
			if d := distanceM(c, pts[0]); d > 400 {
				t.Errorf("the route starts %.0f m from the requested origin", d)
			}
			if d := distanceM(end, pts[len(pts)-1]); d > 400 {
				t.Errorf("the route ends %.0f m from the requested destination", d)
			}
		})
	}
}

func TestRouteStepsAndGeoJSON(t *testing.T) {
	c := center()
	end := geo.Point{Lon: c.Lon + 0.01, Lat: c.Lat + 0.006}
	var res struct {
		Routes []struct {
			Geometry struct {
				Type        string       `json:"type"`
				Coordinates [][2]float64 `json:"coordinates"`
			} `json:"geometry"`
			Legs []struct {
				Steps []struct {
					DistanceM float64 `json:"distance_m"`
					Maneuver  struct {
						Type string  `json:"type"`
						Lat  float64 `json:"lat"`
					} `json:"maneuver"`
				} `json:"steps"`
			} `json:"legs"`
		} `json:"routes"`
	}
	getJSON(t, fmt.Sprintf("/v1/route?profile=%s&steps=true&geometry=geojson&points=%.5f,%.5f;%.5f,%.5f", profiles[0], c.Lon, c.Lat, end.Lon, end.Lat), &res)
	r := res.Routes[0]
	if r.Geometry.Type != "LineString" || len(r.Geometry.Coordinates) < 2 {
		t.Fatalf("geometry: %+v", r.Geometry)
	}
	steps := r.Legs[0].Steps
	if len(steps) < 2 || steps[0].Maneuver.Type != "depart" || steps[len(steps)-1].Maneuver.Type != "arrive" {
		t.Errorf("steps should run from depart to arrive: %+v", steps)
	}
}

func TestRouteValidation(t *testing.T) {
	for name, path := range map[string]string{
		"unknown profile":      "/v1/route?profile=hovercraft&points=71.4,51.1;71.5,51.2",
		"point outside area":   "/v1/route?profile=" + profiles[0] + "&points=71.4,51.1;76.9,43.2",
		"single point":         "/v1/route?profile=" + profiles[0] + "&points=71.4,51.1",
		"reverse out of area":  "/v1/reverse?lat=43.25&lon=76.95",
		"autocomplete no text": "/v1/autocomplete",
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := get(t, path, true)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("HTTP %d, want 400: %s", resp.StatusCode, body)
			}
			var e struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal(body, &e); err != nil || e.Error.Code != "invalid_argument" || e.Error.Message == "" {
				t.Errorf("not the unified error format: %s", body)
			}
		})
	}
}

func TestAuthentication(t *testing.T) {
	resp, body := get(t, "/v1/geocode?q=x", false)
	if resp.StatusCode != http.StatusUnauthorized || !bytes.Contains(body, []byte(`"unauthorized"`)) {
		t.Errorf("without a key: HTTP %d: %s", resp.StatusCode, body)
	}
	for _, public := range []string{"/healthz", "/readyz"} {
		if resp, _ := get(t, public, false); resp.StatusCode != http.StatusOK {
			t.Errorf("%s must be public, got HTTP %d", public, resp.StatusCode)
		}
	}
}

func TestTileZoom14AtCenter(t *testing.T) {
	c := center()
	x, y := geo.TileXY(c, 14)
	resp, body := get(t, fmt.Sprintf("/tiles/14/%d/%d.mvt", x, y), false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.mapbox-vector-tile" {
		t.Errorf("Content-Type = %q", ct)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("ETag") == "" || !strings.Contains(resp.Header.Get("Cache-Control"), "max-age") {
		t.Errorf("headers: %v", resp.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the tile is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil || len(raw) < 1000 {
		t.Fatalf("decompressed tile: %d bytes, %v", len(raw), err)
	}
	// A city-centre tile holds roads and buildings of the OpenMapTiles schema.
	for _, layer := range []string{"transportation", "building", "poi"} {
		if !bytes.Contains(raw, []byte(layer)) {
			t.Errorf("layer %q missing from the tile", layer)
		}
	}
	// Conditional request.
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/tiles/14/%d/%d.mvt", baseURL, x, y), nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	req.Header.Set("Accept-Encoding", "gzip")
	r2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: HTTP %d, want 304", r2.StatusCode)
	}
}

func TestStyleServesEverythingFromTheGateway(t *testing.T) {
	resp, body := get(t, "/tiles/style.json", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("style.json: HTTP %d", resp.StatusCode)
	}
	var style struct {
		Glyphs  string `json:"glyphs"`
		Sprite  string `json:"sprite"`
		Sources map[string]struct {
			Tiles       []string `json:"tiles"`
			Attribution string   `json:"attribution"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(body, &style); err != nil {
		t.Fatal(err)
	}
	src := style.Sources["openmaptiles"]
	if len(src.Tiles) != 1 || !strings.HasSuffix(src.Tiles[0], "/tiles/{z}/{x}/{y}.mvt") {
		t.Errorf("tiles: %v", src.Tiles)
	}
	if !strings.Contains(src.Attribution, "OpenStreetMap contributors") || !strings.Contains(src.Attribution, "OpenMapTiles") {
		t.Errorf("attribution: %q", src.Attribution)
	}
	base := strings.TrimSuffix(src.Tiles[0], "/tiles/{z}/{x}/{y}.mvt")
	for _, u := range []string{style.Glyphs, style.Sprite, src.Tiles[0]} {
		if !strings.HasPrefix(u, base) {
			t.Errorf("%q does not point to the gateway (%s)", u, base)
		}
	}
	// A Cyrillic glyph range and the sprite sheet are really served.
	for _, path := range []string{"/tiles/fonts/Noto%20Sans%20Regular/1024-1279.pbf", "/tiles/sprites/osm-liberty.json", "/tiles/sprites/osm-liberty@2x.png", "/tiles/preview.html"} {
		if r, b := get(t, path, false); r.StatusCode != http.StatusOK || len(b) == 0 {
			t.Errorf("%s: HTTP %d, %d bytes", path, r.StatusCode, len(b))
		}
	}
}

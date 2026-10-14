package osrm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"map-service/internal/geo"
)

// fixture serves a recorded OSRM answer and records the request.
func fixture(t *testing.T, file string, status int) (*Client, *http.Request) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, 2*time.Second), &got
}

var a2b = []geo.Point{{Lon: 71.4306, Lat: 51.1283}, {Lon: 71.47, Lat: 51.18}}

func TestRoutePolylineFixture(t *testing.T) {
	c, req := fixture(t, "route_car_polyline.json", 200)
	res, err := c.Route(context.Background(), RouteParams{Profile: "car", Points: a2b})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Routes) != 1 || res.Routes[0].Distance < 9000 || res.Routes[0].Distance > 9100 {
		t.Fatalf("routes: %+v", res.Routes)
	}
	r := res.Routes[0]
	if r.Duration < 700 || r.Duration > 800 || r.Geometry.Encoded == "" || r.Geometry.Line != nil {
		t.Errorf("route: dist=%v dur=%v geom=%+v", r.Distance, r.Duration, r.Geometry)
	}
	if len(r.Legs) != 1 || len(r.Legs[0].Steps) != 0 {
		t.Errorf("legs: %+v", r.Legs)
	}
	if len(res.Waypoints) != 2 {
		t.Errorf("waypoints: %+v", res.Waypoints)
	}

	// The request must carry the points and the knobs.
	if req.URL.Path != "/route/v1/car/71.4306,51.1283;71.47,51.18" {
		t.Errorf("path = %q", req.URL.Path)
	}
	q := req.URL.Query()
	if q.Get("overview") != "full" || q.Get("geometries") != "polyline" || q.Get("steps") != "false" || q.Get("alternatives") != "false" {
		t.Errorf("query = %v", q)
	}
}

func TestRouteStepsPolyline6(t *testing.T) {
	c, req := fixture(t, "route_foot_steps_polyline6.json", 200)
	res, err := c.Route(context.Background(), RouteParams{Profile: "foot", Points: a2b, Steps: true, Geometry: Polyline6})
	if err != nil {
		t.Fatal(err)
	}
	steps := res.Routes[0].Legs[0].Steps
	if len(steps) != 11 {
		t.Fatalf("steps = %d, want 11", len(steps))
	}
	if steps[1].Maneuver.Type != "turn" || steps[1].Maneuver.Modifier != "left" || steps[1].Maneuver.Location[0] != 71.430618 {
		t.Errorf("maneuver: %+v", steps[1].Maneuver)
	}
	if steps[0].Geometry.Encoded == "" || steps[0].Mode != "walking" {
		t.Errorf("step 0: %+v", steps[0])
	}
	if res.Routes[0].Legs[0].Summary == "" {
		t.Error("leg summary lost")
	}
	if q := req.URL.Query(); q.Get("geometries") != "polyline6" || q.Get("steps") != "true" {
		t.Errorf("query = %v", q)
	}
}

func TestRouteGeoJSON(t *testing.T) {
	c, _ := fixture(t, "route_car_geojson_steps.json", 200)
	res, err := c.Route(context.Background(), RouteParams{Profile: "car", Points: a2b, Steps: true, Geometry: GeoJSON})
	if err != nil {
		t.Fatal(err)
	}
	g := res.Routes[0].Geometry
	if g.Encoded != "" || len(g.Line) != 219 {
		t.Fatalf("geojson geometry: encoded=%q points=%d", g.Encoded, len(g.Line))
	}
	if g.Line[0][0] < 71 || g.Line[0][0] > 72 || g.Line[0][1] < 51 || g.Line[0][1] > 52 {
		t.Errorf("coordinates must be [lon, lat]: %v", g.Line[0])
	}
	if first := res.Routes[0].Legs[0].Steps[0].Geometry; len(first.Line) < 2 {
		t.Errorf("step geometry not decoded: %+v", first)
	}
}

func TestRouteAlternativesAndMultiLeg(t *testing.T) {
	c, req := fixture(t, "route_car_alternatives.json", 200)
	res, err := c.Route(context.Background(), RouteParams{Profile: "car", Points: a2b, Alternatives: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Routes) != 2 || res.Routes[0].Duration > res.Routes[1].Duration {
		t.Errorf("want 2 routes, best first: %+v", res.Routes)
	}
	if req.URL.Query().Get("alternatives") != "true" {
		t.Error("alternatives flag not sent")
	}

	c3, _ := fixture(t, "route_car_three_points.json", 200)
	three := append([]geo.Point{}, a2b[0], geo.Point{Lon: 71.44, Lat: 51.15}, a2b[1])
	r3, err := c3.Route(context.Background(), RouteParams{Profile: "car", Points: three})
	if err != nil || len(r3.Routes[0].Legs) != 2 {
		t.Fatalf("three points: %v %+v", err, r3)
	}
}

func TestRouteErrors(t *testing.T) {
	// NoSegment is a real recorded answer (radiuses=1 on a point away from roads).
	c, _ := fixture(t, "error_no_segment.json", 400)
	_, err := c.Route(context.Background(), RouteParams{Profile: "car", Points: a2b})
	if !errors.Is(err, ErrNoRoute) {
		t.Errorf("NoSegment must map to ErrNoRoute, got %v", err)
	}

	// A bad request surfaces as APIError carrying code and status.
	c2, _ := fixture(t, "error_invalid_value.json", 400)
	_, err = c2.Route(context.Background(), RouteParams{Profile: "car", Points: a2b})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "InvalidValue" || ae.Status != 400 {
		t.Errorf("want APIError InvalidValue/400, got %v", err)
	}
	if errors.Is(err, ErrNoRoute) {
		t.Error("InvalidValue must not be reported as no-route")
	}

	// OSRM documents NoRoute with HTTP 200 and an empty route list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"NoRoute","message":"Impossible route between points","routes":[],"waypoints":[]}`))
	}))
	defer srv.Close()
	_, err = New(srv.URL, time.Second).Route(context.Background(), RouteParams{Profile: "car", Points: a2b})
	if !errors.Is(err, ErrNoRoute) {
		t.Errorf("NoRoute must map to ErrNoRoute, got %v", err)
	}

	if _, err := c.Route(context.Background(), RouteParams{Profile: "car", Points: a2b[:1]}); err == nil {
		t.Error("a single point must be rejected before any request")
	}
}

func TestRouteUpstreamDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := New(srv.URL, time.Second).Route(context.Background(), RouteParams{Profile: "car", Points: a2b})
	if err == nil || errors.Is(err, ErrNoRoute) || !strings.Contains(err.Error(), "500") {
		t.Errorf("want a plain upstream error, got %v", err)
	}
}

func TestHealth(t *testing.T) {
	c, req := fixture(t, "nearest.json", 200)
	if err := c.Health(context.Background(), geo.Point{Lon: 71.4306, Lat: 51.1283}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.URL.Path, "/nearest/v1/driving/71.4306,51.1283") {
		t.Errorf("path = %q", req.URL.Path)
	}
	bad, _ := fixture(t, "error_invalid_value.json", 400)
	if err := bad.Health(context.Background(), geo.Point{}); err == nil {
		t.Error("an error answer must fail the health check")
	}
}

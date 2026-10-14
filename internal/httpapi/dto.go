package httpapi

import "encoding/json"

// Source names the backend that produced a place.
const (
	SourcePhoton    = "photon"
	SourceNominatim = "nominatim"
)

// Address is the structured address of a place. Unknown parts are "".
type Address struct {
	Street      string `json:"street"`
	HouseNumber string `json:"house_number"`
	District    string `json:"district"`
	City        string `json:"city"`
	Postcode    string `json:"postcode"`
}

// Place is the unified geocoding result.
type Place struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Address Address `json:"address"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Type    string  `json:"type"`
	Source  string  `json:"source"`
}

// PlacesResponse is the body of /v1/autocomplete and /v1/geocode.
type PlacesResponse struct {
	Places []Place `json:"places"`
}

// Maneuver is the action at the start of a step.
type Maneuver struct {
	Type     string  `json:"type"`
	Modifier string  `json:"modifier,omitempty"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

// Step is one turn-by-turn instruction.
type Step struct {
	DistanceM float64         `json:"distance_m"`
	DurationS float64         `json:"duration_s"`
	Name      string          `json:"name"`
	Maneuver  Maneuver        `json:"maneuver"`
	Geometry  json.RawMessage `json:"geometry"`
}

// Leg is the part of a route between two consecutive points.
type Leg struct {
	DistanceM float64 `json:"distance_m"`
	DurationS float64 `json:"duration_s"`
	Summary   string  `json:"summary,omitempty"`
	Steps     []Step  `json:"steps,omitempty"`
}

// Route is one way through all requested points. Geometry is a JSON string
// (encoded polyline) or a GeoJSON LineString, depending on ?geometry=.
type Route struct {
	DistanceM float64         `json:"distance_m"`
	DurationS float64         `json:"duration_s"`
	Geometry  json.RawMessage `json:"geometry"`
	Legs      []Leg           `json:"legs"`
}

// Waypoint is a requested point snapped to the road network.
type Waypoint struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

// RouteResponse is the body of /v1/route.
type RouteResponse struct {
	Routes    []Route    `json:"routes"`
	Waypoints []Waypoint `json:"waypoints"`
}

// CheckResult is one entry of the /readyz report.
type CheckResult struct {
	Status    string `json:"status"` // ok | down | disabled
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Readiness is the body of /readyz.
type Readiness struct {
	Status      string                 `json:"status"` // ok | unavailable
	DataVersion string                 `json:"data_version,omitempty"`
	Checks      map[string]CheckResult `json:"checks"`
}

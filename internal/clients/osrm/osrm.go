// Package osrm is a thin client for the OSRM HTTP API (route and nearest).
package osrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"map-service/internal/clients/httpx"
	"map-service/internal/geo"
)

// Geometry selects how OSRM encodes route geometries.
type Geometry string

// Supported geometry encodings.
const (
	Polyline  Geometry = "polyline"  // precision 5
	Polyline6 Geometry = "polyline6" // precision 6
	GeoJSON   Geometry = "geojson"
)

// ErrNoRoute means OSRM found no route, or could not snap a point to the road
// network of the profile.
var ErrNoRoute = errors.New("osrm: no route")

// APIError is an error answer from OSRM that is not ErrNoRoute (usually a
// request we should not have sent: bad coordinates, too many points).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("osrm: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

// Client talks to one OSRM instance (one routing profile).
type Client struct {
	base string
	http *httpx.Client
}

// New creates a client for the OSRM server at baseURL.
func New(baseURL string, timeout time.Duration) *Client {
	return NewWithHTTP(baseURL, httpx.New(timeout))
}

// NewWithHTTP is New with a caller-provided HTTP layer (tests).
func NewWithHTTP(baseURL string, h *httpx.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), http: h}
}

// RouteParams describes a route request.
type RouteParams struct {
	Profile      string // path segment only; the server's data decides the real profile
	Points       []geo.Point
	Alternatives bool
	Steps        bool
	Geometry     Geometry
}

// LineString is a decoded GeoJSON LineString: [lon, lat] pairs.
type LineString [][2]float64

// RouteGeometry is either an encoded polyline or a decoded GeoJSON line,
// depending on the requested Geometry.
type RouteGeometry struct {
	Encoded string
	Line    LineString
}

// UnmarshalJSON accepts both OSRM forms.
func (g *RouteGeometry) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &g.Encoded)
	}
	var ls struct {
		Type        string     `json:"type"`
		Coordinates LineString `json:"coordinates"`
	}
	if err := json.Unmarshal(b, &ls); err != nil {
		return err
	}
	g.Line = ls.Coordinates
	return nil
}

// Maneuver is the action at the start of a step.
type Maneuver struct {
	Type     string     `json:"type"`
	Modifier string     `json:"modifier"`
	Location [2]float64 `json:"location"` // lon, lat
}

// Step is one turn-by-turn instruction.
type Step struct {
	Distance float64       `json:"distance"`
	Duration float64       `json:"duration"`
	Name     string        `json:"name"`
	Mode     string        `json:"mode"`
	Maneuver Maneuver      `json:"maneuver"`
	Geometry RouteGeometry `json:"geometry"`
}

// Leg is the part of a route between two consecutive waypoints.
type Leg struct {
	Distance float64 `json:"distance"`
	Duration float64 `json:"duration"`
	Summary  string  `json:"summary"`
	Steps    []Step  `json:"steps"`
}

// Route is one way through all waypoints.
type Route struct {
	Distance float64       `json:"distance"`
	Duration float64       `json:"duration"`
	Geometry RouteGeometry `json:"geometry"`
	Legs     []Leg         `json:"legs"`
}

// Waypoint is a requested point snapped to the network.
type Waypoint struct {
	Name     string     `json:"name"`
	Location [2]float64 `json:"location"` // lon, lat
	Distance float64    `json:"distance"` // metres from the requested point
}

// RouteResponse is the answer to a route request.
type RouteResponse struct {
	Code      string     `json:"code"`
	Message   string     `json:"message"`
	Routes    []Route    `json:"routes"`
	Waypoints []Waypoint `json:"waypoints"`
}

func coords(points []geo.Point) string {
	var sb strings.Builder
	for i, p := range points {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(strconv.FormatFloat(p.Lon, 'f', -1, 64))
		sb.WriteByte(',')
		sb.WriteString(strconv.FormatFloat(p.Lat, 'f', -1, 64))
	}
	return sb.String()
}

// Route requests a route through p.Points.
func (c *Client) Route(ctx context.Context, p RouteParams) (*RouteResponse, error) {
	if len(p.Points) < 2 {
		return nil, errors.New("osrm: a route needs at least two points")
	}
	if p.Geometry == "" {
		p.Geometry = Polyline
	}
	q := url.Values{}
	q.Set("overview", "full")
	q.Set("geometries", string(p.Geometry))
	q.Set("steps", strconv.FormatBool(p.Steps))
	q.Set("alternatives", strconv.FormatBool(p.Alternatives))
	q.Set("annotations", "false")
	profile := url.PathEscape(p.Profile)
	if profile == "" {
		profile = "driving"
	}
	u := fmt.Sprintf("%s/route/v1/%s/%s?%s", c.base, profile, coords(p.Points), q.Encode())

	var out RouteResponse
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if out.Code != "Ok" {
		return nil, classify(0, out.Code, out.Message)
	}
	if len(out.Routes) == 0 {
		return nil, ErrNoRoute
	}
	return &out, nil
}

// Health checks that the instance answers and has data around pt.
func (c *Client) Health(ctx context.Context, pt geo.Point) error {
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	u := fmt.Sprintf("%s/nearest/v1/driving/%s?number=1", c.base, coords([]geo.Point{pt}))
	if err := c.getJSON(ctx, u, &out); err != nil {
		return err
	}
	if out.Code != "Ok" {
		return classify(0, out.Code, out.Message)
	}
	return nil
}

// getJSON also decodes OSRM's JSON error bodies that come with HTTP 4xx.
func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	b, err := c.http.Get(ctx, u)
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) {
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(b, &e) == nil && e.Code != "" {
				return classify(se.Status, e.Code, e.Message)
			}
		}
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("osrm: decode response: %w", err)
	}
	return nil
}

func classify(status int, code, msg string) error {
	switch code {
	case "NoRoute", "NoSegment":
		return fmt.Errorf("%w: %s", ErrNoRoute, msg)
	}
	return &APIError{Status: status, Code: code, Message: msg}
}

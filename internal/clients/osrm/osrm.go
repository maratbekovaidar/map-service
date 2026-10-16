// Package osrm is a thin client for the OSRM HTTP API (route, match and
// nearest).
package osrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// ErrNoMatch means OSRM could not match a trace to the road network.
var ErrNoMatch = errors.New("osrm: no match")

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

// Bearing restricts the direction in which a point may be left: only road
// segments heading Value degrees (clockwise from north) +/- Range are
// considered for it. The zero Bearing leaves the point unrestricted.
type Bearing struct {
	Value int // 0..359
	Range int // 1..180; 0 means no restriction
}

func (b Bearing) String() string {
	if b.Range <= 0 {
		return ""
	}
	return strconv.Itoa(((b.Value%360)+360)%360) + "," + strconv.Itoa(min(b.Range, 180))
}

// RouteParams describes a route request.
type RouteParams struct {
	Profile      string // path segment only; the server's data decides the real profile
	Points       []geo.Point
	Alternatives bool
	Steps        bool
	Geometry     Geometry
	// Bearings is nil or one entry per point. A bearing on the first point
	// makes a route from a moving vehicle start in its direction of travel
	// instead of with a U-turn.
	Bearings []Bearing
	// Radiuses is nil or one entry per point: how far, in metres, the point may
	// be snapped to the road network; 0 keeps OSRM's default (unlimited).
	Radiuses []float64
	// Annotations adds per-segment distance and duration to every leg.
	Annotations bool
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

// Annotation holds per-segment data of a leg: entry i describes the segment
// between coordinates i and i+1 of the leg's part of the route geometry.
// Durations exclude turn penalties, so they add up to less than the leg's.
type Annotation struct {
	Distance []float64 `json:"distance"`
	Duration []float64 `json:"duration"`
}

// Leg is the part of a route between two consecutive waypoints.
type Leg struct {
	Distance   float64     `json:"distance"`
	Duration   float64     `json:"duration"`
	Summary    string      `json:"summary"`
	Steps      []Step      `json:"steps"`
	Annotation *Annotation `json:"annotation,omitempty"`
}

// Route is one way through all waypoints.
type Route struct {
	Distance float64       `json:"distance"`
	Duration float64       `json:"duration"`
	Geometry RouteGeometry `json:"geometry"`
	Legs     []Leg         `json:"legs"`
}

// SegmentDurations returns the duration of every segment of the route
// geometry (overview=full): the legs' annotations joined together. It is nil
// unless the route was requested with Annotations.
func (r Route) SegmentDurations() []float64 {
	var out []float64
	for _, l := range r.Legs {
		if l.Annotation == nil {
			return nil
		}
		out = append(out, l.Annotation.Duration...)
	}
	return out
}

// Points decodes the geometry, which was requested with encoding g.
func (rg RouteGeometry) Points(g Geometry) ([]geo.Point, error) {
	switch g {
	case GeoJSON:
		pts := make([]geo.Point, len(rg.Line))
		for i, c := range rg.Line {
			pts[i] = geo.Point{Lon: c[0], Lat: c[1]}
		}
		return pts, nil
	case Polyline6:
		return geo.DecodePolyline(rg.Encoded, 6)
	case Polyline, "":
		return geo.DecodePolyline(rg.Encoded, 5)
	}
	return nil, fmt.Errorf("osrm: unknown geometry encoding %q", g)
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

// perPoint joins one value per point with ";" (OSRM's list syntax, where an
// empty item means "default").
func perPoint[T any](name string, points int, vals []T, format func(T) (string, error)) (string, error) {
	if len(vals) != points {
		return "", fmt.Errorf("osrm: %d %s for %d points", len(vals), name, points)
	}
	items := make([]string, len(vals))
	for i, v := range vals {
		s, err := format(v)
		if err != nil {
			return "", fmt.Errorf("osrm: %s %d: %w", name, i, err)
		}
		items[i] = s
	}
	return strings.Join(items, ";"), nil
}

func formatRadius(r float64) (string, error) {
	switch {
	case r < 0 || math.IsNaN(r) || math.IsInf(r, 0):
		return "", fmt.Errorf("invalid radius %v", r)
	case r == 0:
		return "", nil
	}
	return strconv.FormatFloat(r, 'f', -1, 64), nil
}

func formatBearing(b Bearing) (string, error) { return b.String(), nil }

// setPerPoint sets q[name] when vals is non-nil.
func setPerPoint[T any](q url.Values, name string, points int, vals []T, format func(T) (string, error)) error {
	if vals == nil {
		return nil
	}
	s, err := perPoint(name, points, vals, format)
	if err != nil {
		return err
	}
	q.Set(name, s)
	return nil
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
	if p.Annotations {
		q.Set("annotations", "duration,distance")
	}
	if err := setPerPoint(q, "bearings", len(p.Points), p.Bearings, formatBearing); err != nil {
		return nil, err
	}
	if err := setPerPoint(q, "radiuses", len(p.Points), p.Radiuses, formatRadius); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/route/v1/%s/%s?%s", c.base, profilePath(p.Profile), coords(p.Points), q.Encode())

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

func profilePath(profile string) string {
	if profile == "" {
		return "driving"
	}
	return url.PathEscape(profile)
}

// MatchParams describes a map-matching request: a GPS trace to snap onto the
// road network.
type MatchParams struct {
	Profile string // path segment only, as in RouteParams
	Points  []geo.Point
	// Timestamps is nil or one Unix time in seconds per point, strictly
	// increasing: OSRM rejects decreasing ones and fails to match repeated
	// ones, so a trace sampled faster than 1 Hz has to be thinned first.
	Timestamps []int64
	// Radiuses is nil or one entry per point: the GPS accuracy in metres
	// (standard deviation); 0 keeps OSRM's default of 5 m.
	Radiuses []float64
	// Geometry asks for the matched geometry in that encoding; empty leaves
	// it out of the answer.
	Geometry Geometry
}

// Tracepoint is an input point of a match snapped to the road network.
type Tracepoint struct {
	Location          [2]float64 `json:"location"` // lon, lat
	Name              string     `json:"name"`
	Distance          float64    `json:"distance"` // metres from the input point
	MatchingIndex     int        `json:"matchings_index"`
	WaypointIndex     int        `json:"waypoint_index"`
	AlternativesCount int        `json:"alternatives_count"`
}

// Matching is one matched part of a trace (OSRM splits a trace at gaps).
type Matching struct {
	Confidence float64       `json:"confidence"` // 0..1
	Distance   float64       `json:"distance"`
	Duration   float64       `json:"duration"`
	Geometry   RouteGeometry `json:"geometry"`
	Legs       []Leg         `json:"legs"`
}

// MatchResponse is the answer to a match request.
type MatchResponse struct {
	Code      string     `json:"code"`
	Message   string     `json:"message"`
	Matchings []Matching `json:"matchings"`
	// Tracepoints has one entry per input point, nil for points OSRM dropped
	// as outliers.
	Tracepoints []*Tracepoint `json:"tracepoints"`
}

// Match snaps the trace p.Points onto the road network.
func (c *Client) Match(ctx context.Context, p MatchParams) (*MatchResponse, error) {
	if len(p.Points) < 2 {
		return nil, errors.New("osrm: a match needs at least two points")
	}
	q := url.Values{}
	q.Set("steps", "false")
	q.Set("overview", "false")
	if p.Geometry != "" {
		q.Set("overview", "full")
		q.Set("geometries", string(p.Geometry))
	}
	for i := 1; i < len(p.Timestamps); i++ {
		if p.Timestamps[i] <= p.Timestamps[i-1] {
			return nil, fmt.Errorf("osrm: timestamp %d is not after the previous one", i)
		}
	}
	formatTS := func(ts int64) (string, error) { return strconv.FormatInt(ts, 10), nil }
	if err := setPerPoint(q, "timestamps", len(p.Points), p.Timestamps, formatTS); err != nil {
		return nil, err
	}
	if err := setPerPoint(q, "radiuses", len(p.Points), p.Radiuses, formatRadius); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/match/v1/%s/%s?%s", c.base, profilePath(p.Profile), coords(p.Points), q.Encode())

	var out MatchResponse
	err := c.getJSON(ctx, u, &out)
	if err == nil && out.Code != "Ok" {
		err = classify(0, out.Code, out.Message)
	}
	switch {
	case errors.Is(err, ErrNoRoute): // NoSegment: a point too far from any road
		return nil, fmt.Errorf("%w: %w", ErrNoMatch, err)
	case err != nil:
		return nil, err
	case len(out.Matchings) == 0:
		return nil, ErrNoMatch
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
	case "NoMatch":
		return fmt.Errorf("%w: %s", ErrNoMatch, msg)
	}
	return &APIError{Status: status, Code: code, Message: msg}
}

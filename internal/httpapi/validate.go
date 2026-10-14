package httpapi

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"map-service/internal/clients/osrm"
	"map-service/internal/config"
	"map-service/internal/geo"
)

const (
	maxQueryRunes = 200
	defaultLimit  = 5
)

// queryValues parses the URL query like url.ParseQuery, with one deliberate
// difference: only '&' separates parameters. Go's parser treats ';' as an
// error and silently drops the pair, which would swallow the documented
// points=lon,lat;lon,lat when a client does not percent-encode the ';'.
func queryValues(r *http.Request) (url.Values, *Error) {
	vals := url.Values{}
	for _, pair := range strings.Split(r.URL.RawQuery, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		k, errK := url.QueryUnescape(k)
		v, errV := url.QueryUnescape(v)
		if errK != nil || errV != nil {
			return nil, badRequest("the query string contains an invalid percent-escape")
		}
		vals.Add(k, v)
	}
	return vals, nil
}

// validator holds the limits that request parameters are checked against.
type validator struct {
	bbox       geo.BBox
	languages  []string // lower-case, first is the default
	profiles   []string
	maxResults int
	maxPoints  int
}

func newValidator(cfg *config.Gateway) *validator {
	langs := make([]string, len(cfg.Languages))
	for i, l := range cfg.Languages {
		langs[i] = strings.ToLower(l)
	}
	return &validator{bbox: cfg.BBox, languages: langs, profiles: cfg.Profiles, maxResults: cfg.MaxResults, maxPoints: cfg.MaxRoutePoints}
}

func (v *validator) query(vals url.Values) (string, *Error) {
	q := strings.TrimSpace(vals.Get("q"))
	if q == "" {
		return "", badRequest("q: is required")
	}
	if n := utf8.RuneCountInString(q); n > maxQueryRunes {
		return "", badRequest("q: must be at most %d characters", maxQueryRunes)
	}
	if strings.ContainsFunc(q, unicode.IsControl) {
		return "", badRequest("q: must not contain control characters")
	}
	return q, nil
}

func (v *validator) limit(vals url.Values) (int, *Error) {
	s := vals.Get("limit")
	if s == "" {
		return min(defaultLimit, v.maxResults), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > v.maxResults {
		return 0, badRequest("limit: must be an integer between 1 and %d", v.maxResults)
	}
	return n, nil
}

// lang returns the normalised language (lower-case, one of LANGUAGES).
func (v *validator) lang(vals url.Values) (string, *Error) {
	s := strings.ToLower(strings.TrimSpace(vals.Get("lang")))
	if s == "" {
		return v.languages[0], nil
	}
	if !slices.Contains(v.languages, s) {
		return "", badRequest("lang: must be one of %s", strings.Join(v.languages, ", "))
	}
	return s, nil
}

// langPriority lists lang first, then the other configured languages; it is
// what Nominatim's accept-language gets.
func (v *validator) langPriority(lang string) []string {
	out := []string{lang}
	for _, l := range v.languages {
		if l != lang {
			out = append(out, l)
		}
	}
	return out
}

func parseCoord(vals url.Values, name string, lo, hi float64) (float64, *Error) {
	s := vals.Get(name)
	if s == "" {
		return 0, badRequest("%s: is required", name)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != f || f < lo || f > hi { // f != f rejects NaN; range rejects ±Inf
		return 0, badRequest("%s: must be a number between %g and %g", name, lo, hi)
	}
	return f, nil
}

// point reads lat and lon, both required, and checks them against the service area.
func (v *validator) point(vals url.Values) (geo.Point, *Error) {
	lat, e := parseCoord(vals, "lat", -90, 90)
	if e != nil {
		return geo.Point{}, e
	}
	lon, e := parseCoord(vals, "lon", -180, 180)
	if e != nil {
		return geo.Point{}, e
	}
	p := geo.Point{Lon: lon, Lat: lat}
	if !v.bbox.Contains(p) {
		return geo.Point{}, badRequest("lat/lon: the point is outside the service area (%s)", v.bbox)
	}
	return p, nil
}

// optionalBias reads the optional lat/lon location bias: both or neither.
func (v *validator) optionalBias(vals url.Values) (*geo.Point, *Error) {
	hasLat, hasLon := vals.Get("lat") != "", vals.Get("lon") != ""
	switch {
	case !hasLat && !hasLon:
		return nil, nil
	case hasLat != hasLon:
		return nil, badRequest("lat and lon must be given together")
	}
	lat, e := parseCoord(vals, "lat", -90, 90)
	if e != nil {
		return nil, e
	}
	lon, e := parseCoord(vals, "lon", -180, 180)
	if e != nil {
		return nil, e
	}
	return &geo.Point{Lon: lon, Lat: lat}, nil
}

func parseBool(vals url.Values, name string) (bool, *Error) {
	s := vals.Get(name)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, badRequest("%s: must be true or false", name)
	}
	return b, nil
}

func (v *validator) profile(vals url.Values) (string, *Error) {
	p := vals.Get("profile")
	if p == "" {
		return "", badRequest("profile: is required")
	}
	if !slices.Contains(v.profiles, p) {
		return "", badRequest("profile: must be one of %s", strings.Join(v.profiles, ", "))
	}
	return p, nil
}

func (v *validator) geometry(vals url.Values) (osrm.Geometry, *Error) {
	switch g := osrm.Geometry(vals.Get("geometry")); g {
	case "":
		return osrm.Polyline, nil
	case osrm.Polyline, osrm.Polyline6, osrm.GeoJSON:
		return g, nil
	default:
		return "", badRequest("geometry: must be polyline, polyline6 or geojson")
	}
}

// points parses "lon,lat;lon,lat[;...]" and checks count, range and service area.
func (v *validator) points(s string) ([]geo.Point, *Error) {
	if strings.TrimSpace(s) == "" {
		return nil, badRequest("points: is required, e.g. points=lon,lat;lon,lat")
	}
	parts := strings.Split(s, ";")
	if len(parts) < 2 {
		return nil, badRequest("points: at least two points are required")
	}
	if len(parts) > v.maxPoints {
		return nil, badRequest("points: at most %d points are allowed", v.maxPoints)
	}
	out := make([]geo.Point, 0, len(parts))
	for i, part := range parts {
		p, err := geo.ParsePoint(part)
		if err != nil {
			return nil, badRequest("points[%d]: must be lon,lat with lon in [-180,180] and lat in [-90,90]", i)
		}
		if !v.bbox.Contains(p) {
			return nil, badRequest("points[%d]: the point is outside the service area (%s)", i, v.bbox)
		}
		out = append(out, p)
	}
	return out, nil
}

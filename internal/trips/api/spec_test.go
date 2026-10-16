package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"map-service/api"
	"map-service/internal/config"
	"map-service/internal/trips/store"
	"map-service/internal/trips/tracker"
)

// The trips part of api/openapi.yaml must describe this service: its paths
// are routed here, its schemas have exactly the fields of the DTOs, its
// enums the values the code produces. None of this needs a database.

type specSchema struct {
	Required   []string                  `yaml:"required"`
	Properties map[string]map[string]any `yaml:"properties"`
	Enum       []string                  `yaml:"enum"`
}

type tripsSpec struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]specSchema `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) tripsSpec {
	t.Helper()
	var s tripsSpec
	if err := yaml.Unmarshal(api.OpenAPI, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpecTripPathsAreRouted(t *testing.T) {
	cfg, err := config.LoadTrips(func(k string) string { return map[string]string{"API_KEYS": testKey}[k] })
	if err != nil {
		t.Fatal(err)
	}
	// Without credentials every trips endpoint answers 401 before touching
	// the database, which proves that the route exists.
	h := New(Deps{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	n := 0
	for p, item := range loadSpec(t).Paths {
		if !strings.HasPrefix(p, "/v1/trips") {
			continue
		}
		for method := range item {
			method = strings.ToUpper(method)
			if method != "GET" && method != "POST" {
				continue
			}
			path := strings.ReplaceAll(p, "{id}", "01a14369-9f1b-7ba3-87a8-926e582553f9")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("spec documents %s %s; the service answers %d %s", method, p, rec.Code, rec.Body.String())
			}
			n++
		}
	}
	if n != 7 {
		t.Errorf("%d trips operations in the spec, want 7", n)
	}
}

// jsonFields lists the JSON names of a struct's fields (embedded structs
// included) and which of them are omitempty.
func jsonFields(t reflect.Type) (names []string, optional map[string]bool) {
	optional = map[string]bool{}
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if f.Anonymous && tag == "" {
			n, o := jsonFields(f.Type)
			names = append(names, n...)
			for k, v := range o {
				optional[k] = v
			}
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
		optional[name] = strings.Contains(opts, "omitempty")
	}
	return names, optional
}

func TestSpecSchemasMatchTheDTOs(t *testing.T) {
	schemas := loadSpec(t).Components.Schemas
	for name, v := range map[string]any{
		"LatLon": LatLon{}, "CreateTripRequest": CreateTripRequest{}, "Trip": TripDTO{}, "TripRoute": RouteDTO{},
		"CreateTripResponse": CreateTripResponse{}, "PositionInput": PositionIn{}, "PositionsRequest": PositionsRequest{},
		"Progress": Progress{}, "PositionsResponse": PositionsResponse{}, "TripPosition": PositionDTO{},
		"TripSnapshot": SnapshotResponse{}, "TripPoint": PointDTO{}, "PositionEvent": PositionEvent{},
		"TripStatusEvent": store.StatusEvent{}, "TripEnd": EndResponse{}, "TrackPosition": TrackPosition{},
		"TrackResponse": TrackResponse{},
	} {
		sc, ok := schemas[name]
		if !ok {
			t.Errorf("schema %s is missing", name)
			continue
		}
		fields, optional := jsonFields(reflect.TypeOf(v))
		var props []string
		for p := range sc.Properties {
			props = append(props, p)
		}
		sort.Strings(fields)
		sort.Strings(props)
		if !slices.Equal(fields, props) {
			t.Errorf("%s: the DTO has %v, the spec %v", name, fields, props)
		}
		for _, r := range sc.Required {
			if optional[r] {
				t.Errorf("%s: %s is required in the spec but omitted when empty", name, r)
			}
		}
	}
}

func TestSpecEnumsMatchTheCode(t *testing.T) {
	sc := loadSpec(t).Components.Schemas
	enum := func(schema, prop string) []string {
		if prop == "" {
			return sorted(sc[schema].Enum)
		}
		var out []string
		for _, v := range sc[schema].Properties[prop]["enum"].([]any) {
			out = append(out, v.(string))
		}
		return sorted(out)
	}
	for _, c := range []struct {
		name string
		spec []string
		code []string
	}{
		{"trip status", enum("TripStatus", ""), []string{string(store.Active), string(store.Arrived), string(store.Finished), string(store.Cancelled), string(store.Expired)}},
		{"route reason", enum("TripRoute", "reason"), []string{store.ReasonInitial, tracker.ReasonOffRoute, tracker.ReasonWrongWay}},
		{"position class", enum("TrackPosition", "class"), []string{string(tracker.ClassOn), string(tracker.ClassNear), string(tracker.ClassOff), string(tracker.ClassWrongWay), string(tracker.ClassStart)}},
		{"reject reason", enum("TrackPosition", "reject"), []string{tracker.RejectDuplicate, tracker.RejectOutOfOrder, tracker.RejectInvalid, tracker.RejectInaccurate, tracker.RejectJump, rejectBadTime}},
	} {
		if code := sorted(c.code); !slices.Equal(c.spec, code) {
			t.Errorf("%s: the spec lists %v, the code produces %v", c.name, c.spec, code)
		}
	}
	codes := sc["Error"].Properties["error"]["properties"].(map[string]any)["code"].(map[string]any)["enum"].([]any)
	if !slices.Contains(codes, any(CodeTripEnded)) {
		t.Errorf("error codes %v lack %s", codes, CodeTripEnded)
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

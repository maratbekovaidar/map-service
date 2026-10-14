package httpapi

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"map-service/api"
)

type specDoc struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title       string `yaml:"title"`
		Description string `yaml:"description"`
	} `yaml:"info"`
	Servers []struct {
		URL         string `yaml:"url"`
		Description string `yaml:"description"`
	} `yaml:"servers"`
	Paths      map[string]any `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]any `yaml:"securitySchemes"`
	} `yaml:"components"`
}

func parseSpec(t *testing.T, b []byte) specDoc {
	t.Helper()
	var d specDoc
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatalf("spec is not valid YAML: %v", err)
	}
	return d
}

func TestOpenAPIPointsAtThisServer(t *testing.T) {
	hs := newHarness(t, nil, nil)
	rec := hs.do("GET", "/openapi.yaml")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("openapi: %d %v", rec.Code, rec.Header())
	}
	got, orig := parseSpec(t, rec.Body.Bytes()), parseSpec(t, api.OpenAPI)
	if len(got.Servers) != 1 || got.Servers[0].URL != "https://maps.example.com" {
		t.Errorf("servers = %+v, want only PUBLIC_BASE_URL", got.Servers)
	}
	if len(orig.Servers) < 2 {
		t.Errorf("the committed spec is expected to list its own servers, got %+v", orig.Servers)
	}
	// Everything but `servers` is served as committed.
	if got.OpenAPI != orig.OpenAPI || got.Info != orig.Info || len(got.Paths) != len(orig.Paths) ||
		len(got.Components.SecuritySchemes) != len(orig.Components.SecuritySchemes) {
		t.Errorf("spec content changed beyond servers")
	}
	if !strings.Contains(rec.Body.String(), "**Authentication.**") {
		t.Errorf("block scalars of the description must survive the rewrite")
	}
}

func TestOpenAPIDerivesServerFromRequest(t *testing.T) {
	hs := newHarness(t, map[string]string{"PUBLIC_BASE_URL": ""}, nil)
	rec := hs.do("GET", "/openapi.yaml")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := parseSpec(t, rec.Body.Bytes()); len(got.Servers) != 1 || got.Servers[0].URL != "http://example.com" {
		t.Errorf("servers = %+v, want the request host", got.Servers)
	}
}

func TestSpecForServerAddsMissingServers(t *testing.T) {
	out, err := specForServer([]byte("openapi: 3.1.0\ninfo:\n  title: t\n  version: '1'\npaths: {}\n"), "https://x.test")
	if err != nil {
		t.Fatal(err)
	}
	if got := parseSpec(t, out); len(got.Servers) != 1 || got.Servers[0].URL != "https://x.test" {
		t.Errorf("servers = %+v", got.Servers)
	}
	if _, err := specForServer([]byte("a: [unclosed"), "https://x.test"); err == nil {
		t.Error("invalid YAML must be an error")
	}
}

func TestDocsPage(t *testing.T) {
	hs := newHarness(t, nil, nil)
	rec := hs.do("GET", "/docs")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("docs: %d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{"docs/swagger-ui-bundle.js", "docs/swagger-ui.css", "openapi.yaml"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("docs page does not reference %s", want)
		}
	}
	// The page is cacheable: a repeat request with the ETag is a 304.
	again := hs.do("GET", "/docs", "If-None-Match", rec.Header().Get("ETag"))
	if again.Code != http.StatusNotModified {
		t.Errorf("revalidation: %d", again.Code)
	}
	wantError(t, hs.do("POST", "/docs"), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
}

func TestDocsAssets(t *testing.T) {
	hs := newHarness(t, nil, nil)
	writeFile(t, filepath.Join(hs.static, "vendor", "swagger-ui-bundle.js"), []byte("/*js*/"))
	writeFile(t, filepath.Join(hs.static, "vendor", "swagger-ui.css"), []byte("/*css*/"))
	writeFile(t, filepath.Join(hs.static, "vendor", "maplibre-gl.js"), []byte("/*map*/"))
	writeFile(t, filepath.Join(hs.static, "vendor", "secret.txt"), []byte("nope"))

	js := hs.do("GET", "/docs/swagger-ui-bundle.js")
	if js.Code != 200 || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") || js.Body.String() != "/*js*/" {
		t.Errorf("bundle: %d %v", js.Code, js.Header())
	}
	css := hs.do("GET", "/docs/swagger-ui.css")
	if css.Code != 200 || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Errorf("css: %d %v", css.Code, css.Header())
	}
	// Only the Swagger UI files are served here, not the rest of vendor/.
	for _, target := range []string{"/docs/maplibre-gl.js", "/docs/secret.txt", "/docs/..%2fstyle.json"} {
		wantError(t, hs.do("GET", target), http.StatusNotFound, CodeNotFound)
	}
	// ... and Swagger UI is not exposed under the tiles' vendor route.
	wantError(t, hs.do("GET", "/tiles/vendor/swagger-ui-bundle.js"), http.StatusNotFound, CodeNotFound)
}

func TestDocsAreNotBehindAPIKeys(t *testing.T) {
	hs := newHarness(t, map[string]string{"AUTH_REQUIRE_FOR_TILES": "true"}, nil)
	writeFile(t, filepath.Join(hs.static, "vendor", "swagger-ui.css"), []byte("/*css*/"))
	for _, target := range []string{"/docs", "/docs/swagger-ui.css", "/openapi.yaml"} {
		if rec := hs.do("GET", target); rec.Code != 200 {
			t.Errorf("%s without a key: %d", target, rec.Code)
		}
	}
	wantError(t, hs.do("GET", "/v1/geocode?q=x"), http.StatusUnauthorized, CodeUnauthorized)
}

// TestSpecPathsAreRouted keeps the documentation honest: every concrete path
// of the spec must exist in the gateway (templated tile paths are covered by
// the tile tests).
func TestSpecPathsAreRouted(t *testing.T) {
	hs := newHarness(t, nil, nil)
	spec := parseSpec(t, api.OpenAPI)
	for p := range spec.Paths {
		if strings.Contains(p, "{") {
			continue
		}
		rec := hs.do("GET", p)
		if rec.Code == http.StatusMethodNotAllowed || strings.Contains(rec.Body.String(), "no such endpoint") {
			t.Errorf("spec documents GET %s but the gateway has no such route (%d)", p, rec.Code)
		}
	}
}

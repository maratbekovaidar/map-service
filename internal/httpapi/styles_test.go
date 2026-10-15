package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const adminKey = "admin-key"

func styleDoc(name, desc string) []byte {
	return []byte(`{"version":8,"name":"` + name + `","metadata":{"map-service:description":"` + desc + `"},` +
		`"sources":{"openmaptiles":{"type":"vector","tiles":["{{PUBLIC_BASE_URL}}/tiles/{z}/{x}/{y}.mvt"]}},"layers":[]}`)
}

// stylesHarness installs two styles; env may set ADMIN_API_KEYS, STATE_DIR...
func stylesHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	hs := newHarness(t, env, nil)
	writeFile(t, filepath.Join(hs.static, "styles", "liberty.json"), styleDoc("OSM Liberty", "classic"))
	writeFile(t, filepath.Join(hs.static, "styles", "warm.json"), styleDoc("Warm", "warm"))
	return hs
}

// put sends PUT /v1/styles/default with body and key.
func (hs *harness) put(target, body, key string) *httptest.ResponseRecorder {
	hs.t.Helper()
	req := httptest.NewRequest("PUT", target, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func TestListStyles(t *testing.T) {
	hs := stylesHarness(t, nil)
	wantError(t, hs.do("GET", "/v1/styles"), http.StatusUnauthorized, CodeUnauthorized)
	rec := hs.get("/v1/styles")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d %v: %s", rec.Code, rec.Header(), rec.Body)
	}
	resp := decode[StylesResponse](t, rec)
	if resp.Default != "liberty" || len(resp.Styles) != 2 {
		t.Fatalf("response: %+v", resp)
	}
	lib, warm := resp.Styles[0], resp.Styles[1]
	if warm.ID != "warm" || warm.Name != "Warm" || warm.Description != "warm" || warm.Default ||
		warm.URL != "https://maps.example.com/tiles/styles/warm.json" {
		t.Errorf("warm: %+v", warm)
	}
	if lib.ID != "liberty" || !lib.Default {
		t.Errorf("liberty: %+v", lib)
	}
}

func TestStyleByID(t *testing.T) {
	hs := stylesHarness(t, nil)
	rec := hs.do("GET", "/tiles/styles/warm.json")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Warm"`) ||
		!strings.Contains(rec.Body.String(), `"https://maps.example.com/tiles/{z}/{x}/{y}.mvt"`) {
		t.Fatalf("style by id: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
	for _, target := range []string{"/tiles/styles/missing.json", "/tiles/styles/liberty", "/tiles/styles/..%2fpreview.html", "/tiles/styles/Liberty.json"} {
		wantError(t, hs.do("GET", target), http.StatusNotFound, CodeNotFound)
	}
}

func TestDefaultStyleIsRevalidated(t *testing.T) {
	hs := stylesHarness(t, map[string]string{"DEFAULT_STYLE": "warm"})
	rec := hs.do("GET", "/tiles/style.json")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Warm"`) {
		t.Fatalf("DEFAULT_STYLE not served: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("the default style can change at runtime, Cache-Control = %q", cc)
	}
}

func TestSetDefaultStyle(t *testing.T) {
	state := t.TempDir()
	env := map[string]string{"ADMIN_API_KEYS": adminKey, "STATE_DIR": state}
	hs := stylesHarness(t, env)
	before := hs.do("GET", "/tiles/style.json")

	// Client keys may not switch; unknown/invalid bodies are rejected.
	wantError(t, hs.put("/v1/styles/default", `{"id":"warm"}`, ""), http.StatusUnauthorized, CodeUnauthorized)
	wantError(t, hs.put("/v1/styles/default", `{"id":"warm"}`, testKey), http.StatusUnauthorized, CodeUnauthorized)
	wantError(t, hs.put("/v1/styles/default", `{"id":"nope"}`, adminKey), http.StatusBadRequest, CodeInvalidArgument)
	wantError(t, hs.put("/v1/styles/default", `{"id":"../liberty"}`, adminKey), http.StatusBadRequest, CodeInvalidArgument)
	wantError(t, hs.put("/v1/styles/default", `{"style":"liberty"}`, adminKey), http.StatusBadRequest, CodeInvalidArgument)
	wantError(t, hs.put("/v1/styles/default", `not json`, adminKey), http.StatusBadRequest, CodeInvalidArgument)
	wantError(t, hs.put("/v1/styles/default", `{"id":"`+strings.Repeat("a", 5000)+`"}`, adminKey), http.StatusBadRequest, CodeInvalidArgument)

	rec := hs.put("/v1/styles/default", `{"id":"warm"}`, adminKey)
	if rec.Code != 200 {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body)
	}
	if resp := decode[StylesResponse](t, rec); resp.Default != "warm" || resp.Styles[0].Default || !resp.Styles[1].Default {
		t.Errorf("response after switch: %+v", resp)
	}
	after := hs.do("GET", "/tiles/style.json")
	if !strings.Contains(after.Body.String(), `"Warm"`) || after.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Errorf("style.json must serve the new default with a new ETag:\n%s", after.Body)
	}
	if r := hs.do("GET", "/tiles/style.json", "If-None-Match", before.Header().Get("ETag")); r.Code != 200 {
		t.Errorf("a client holding the old style must get the new one, got %d", r.Code)
	}
	if !strings.Contains(hs.logs.String(), `"msg":"default style changed"`) || strings.Contains(hs.logs.String(), adminKey) {
		t.Errorf("the switch must be logged without the key:\n%s", hs.logs)
	}

	// The admin key works as a client key too.
	if r := hs.do("GET", "/v1/styles", "X-API-Key", adminKey); r.Code != 200 {
		t.Errorf("admin key on /v1: %d", r.Code)
	}

	// A restarted gateway keeps the choice.
	hs2 := newHarness(t, env, nil)
	writeFile(t, filepath.Join(hs2.static, "styles", "warm.json"), styleDoc("Warm", "warm"))
	writeFile(t, filepath.Join(hs2.static, "styles", "liberty.json"), styleDoc("OSM Liberty", "classic"))
	if r := hs2.do("GET", "/tiles/style.json"); !strings.Contains(r.Body.String(), `"Warm"`) {
		t.Errorf("after restart:\n%s", r.Body)
	}
}

func TestSetDefaultStyleDisabledWithoutAdminKeys(t *testing.T) {
	// Not even AUTH_DISABLED opens it.
	hs := stylesHarness(t, map[string]string{"AUTH_DISABLED": "true"})
	wantError(t, hs.put("/v1/styles/default", `{"id":"warm"}`, testKey), http.StatusForbidden, CodeForbidden)
	if r := hs.do("GET", "/v1/styles"); r.Code != 200 {
		t.Errorf("listing with AUTH_DISABLED: %d", r.Code)
	}
}

func TestSetDefaultStyleMethodAndCORS(t *testing.T) {
	hs := stylesHarness(t, map[string]string{"ADMIN_API_KEYS": adminKey})
	rec := hs.get("/v1/styles/default")
	wantError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != "PUT, OPTIONS" {
		t.Errorf("Allow = %q", allow)
	}
	pre := hs.do("OPTIONS", "/v1/styles/default", "Origin", "https://app.example", "Access-Control-Request-Method", "PUT")
	if !strings.Contains(pre.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Errorf("preflight must allow PUT: %v", pre.Header())
	}
}

func TestNoDefaultStyleInstalled(t *testing.T) {
	hs := newHarness(t, map[string]string{"DEFAULT_STYLE": "missing"}, nil)
	writeFile(t, filepath.Join(hs.static, "styles", "liberty.json"), styleDoc("OSM Liberty", ""))
	wantError(t, hs.do("GET", "/tiles/style.json"), http.StatusNotFound, CodeNotFound)
	if resp := decode[StylesResponse](t, hs.get("/v1/styles")); resp.Default != "" || len(resp.Styles) != 1 || resp.Styles[0].Default {
		t.Errorf("listing without a default: %+v", resp)
	}
}

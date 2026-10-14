package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"map-service/internal/geo"
	"map-service/internal/tiles"
)

// ------------------------------------------------------------------- probes

func TestHealthz(t *testing.T) {
	hs := newHarness(t, nil, nil)
	rec := hs.do("GET", "/healthz")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body)
	}
}

func TestReadyzAllOK(t *testing.T) {
	hs := newHarness(t, nil, nil)
	rec := hs.do("GET", "/readyz")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decode[Readiness](t, rec)
	if got.Status != "ok" || got.DataVersion != "20261014T000000Z" {
		t.Errorf("readiness: %+v", got)
	}
	for _, name := range []string{"photon", "nominatim", "osrm-car", "osrm-foot", "tiles"} {
		if c, ok := got.Checks[name]; !ok || c.Status != "ok" {
			t.Errorf("check %q: %+v", name, c)
		}
	}
	if got.Checks["redis"].Status != "disabled" {
		t.Errorf("redis without cache must be reported as disabled: %+v", got.Checks["redis"])
	}
}

func TestReadyzReportsDownUpstreamsWithoutLeakingDetails(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.photon.health = func(context.Context) error {
		return errors.New("dial tcp 172.18.0.7:2322: connect: connection refused")
	}
	hs.foot.health = func(context.Context, geo.Point) error { return context.DeadlineExceeded }
	rec := hs.do("GET", "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body)
	}
	got := decode[Readiness](t, rec)
	if got.Status != "unavailable" {
		t.Errorf("status = %q", got.Status)
	}
	if c := got.Checks["photon"]; c.Status != "down" || c.Error != "unavailable" {
		t.Errorf("photon: %+v", c)
	}
	if c := got.Checks["osrm-foot"]; c.Status != "down" || c.Error != "timeout" {
		t.Errorf("osrm-foot: %+v", c)
	}
	if c := got.Checks["osrm-car"]; c.Status != "ok" {
		t.Errorf("healthy checks must still be reported ok: %+v", c)
	}
	if strings.Contains(rec.Body.String(), "172.18") || strings.Contains(rec.Body.String(), "refused") {
		t.Errorf("public /readyz leaks internal details: %s", rec.Body)
	}
	if !strings.Contains(hs.logs.String(), "connection refused") {
		t.Error("the details must be logged instead")
	}
}

func TestReadyzTilesArchiveMissing(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.tiles.ready = func(version string) error {
		if version != "current" {
			t.Errorf("readiness must check the live archive, got %q", version)
		}
		return errors.New("archive not found")
	}
	rec := hs.do("GET", "/readyz")
	if rec.Code != http.StatusServiceUnavailable || decode[Readiness](t, rec).Checks["tiles"].Status != "down" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestReadyzRedisIsOptional(t *testing.T) {
	store, mr := newMiniredisStore(t)
	hs := newHarness(t, nil, store)
	if rec := hs.do("GET", "/readyz"); decode[Readiness](t, rec).Checks["redis"].Status != "ok" {
		t.Fatalf("redis should be ok: %s", rec.Body)
	}
	mr.Close()
	hs.srv.ready.at = hs.srv.ready.at.AddDate(0, 0, -1) // expire the 1s report cache
	rec := hs.do("GET", "/readyz")
	got := decode[Readiness](t, rec)
	if rec.Code != 200 || got.Status != "ok" || got.Checks["redis"].Status != "down" {
		t.Errorf("a dead Redis must be shown but must not make the gateway unready: %d %+v", rec.Code, got)
	}
}

func TestReadyzResultIsBrieflyCached(t *testing.T) {
	hs := newHarness(t, nil, nil)
	calls := 0
	hs.photon.health = func(context.Context) error { calls++; return nil }
	for i := 0; i < 5; i++ {
		hs.do("GET", "/readyz")
	}
	if calls != 1 {
		t.Errorf("a burst of probes must reuse one report, checks ran %d times", calls)
	}
}

// -------------------------------------------------------------------- tiles

func TestTileOK(t *testing.T) {
	hs := newHarness(t, nil, nil)
	var gotVersion string
	var gotZXY [3]int
	hs.tiles.tile = func(version string, z, x, y int) tiles.Result {
		gotVersion, gotZXY = version, [3]int{z, x, y}
		return tiles.Result{Status: 200, Body: []byte("\x1f\x8bGZIPPED"), Headers: map[string]string{
			"ETag": `"abc123"`, "Content-Encoding": "gzip", "Content-Type": "application/x-protobuf"}}
	}
	rec := hs.do("GET", "/tiles/14/11442/5475.mvt")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if gotVersion != "current" || gotZXY != [3]int{14, 11442, 5475} {
		t.Errorf("tile request: %q %v", gotVersion, gotZXY)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/vnd.mapbox-vector-tile" || h.Get("Content-Encoding") != "gzip" ||
		h.Get("ETag") != `"abc123"` || h.Get("Cache-Control") != "public, max-age=3600" || h.Get("Content-Length") != "9" {
		t.Errorf("headers: %v", h)
	}
	if rec.Body.String() != "\x1f\x8bGZIPPED" {
		t.Errorf("body: %q", rec.Body.String())
	}

	// Conditional request.
	for _, inm := range []string{`"abc123"`, `W/"abc123"`, `"zzz", "abc123"`, `*`} {
		r304 := hs.do("GET", "/tiles/14/11442/5475.mvt", "If-None-Match", inm)
		if r304.Code != http.StatusNotModified || r304.Body.Len() != 0 || r304.Header().Get("ETag") != `"abc123"` {
			t.Errorf("If-None-Match %s: %d body=%q", inm, r304.Code, r304.Body)
		}
	}
	if r := hs.do("GET", "/tiles/14/11442/5475.mvt", "If-None-Match", `"stale"`); r.Code != 200 {
		t.Errorf("a stale validator must get the tile: %d", r.Code)
	}
}

func TestTileCacheControlIsConfigurable(t *testing.T) {
	hs := newHarness(t, map[string]string{"TILES_CACHE_MAX_AGE": "24h"}, nil)
	hs.tiles.tile = func(string, int, int, int) tiles.Result {
		return tiles.Result{Status: 200, Body: []byte("x"), Headers: map[string]string{"ETag": `"e"`}}
	}
	if got := hs.do("GET", "/tiles/0/0/0.mvt").Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestTileEmptyAndErrors(t *testing.T) {
	hs := newHarness(t, nil, nil)
	hs.tiles.tile = func(string, int, int, int) tiles.Result { return tiles.Result{Status: 204} }
	rec := hs.do("GET", "/tiles/10/1/1.mvt")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") == "" {
		t.Errorf("empty tile: %d %v", rec.Code, rec.Header())
	}
	for status, body := range map[int]string{404: "Archive not found", 500: "I/O Error"} {
		hs.tiles.tile = func(string, int, int, int) tiles.Result { return tiles.Result{Status: status, Body: []byte(body)} }
		e := wantError(t, hs.do("GET", "/tiles/10/1/1.mvt"), http.StatusBadGateway, CodeUpstreamUnavailable)
		if strings.Contains(e.Message, body) {
			t.Errorf("internal detail leaked: %q", e.Message)
		}
	}
}

func TestTileBadCoordinates(t *testing.T) {
	hs := newHarness(t, nil, nil)
	for _, target := range []string{
		"/tiles/15/0/0.mvt",       // above the archive's max zoom
		"/tiles/3/8/0.mvt",        // x outside the grid
		"/tiles/3/0/8.mvt",        // y outside the grid
		"/tiles/a/0/0.mvt",        // not numbers
		"/tiles/3/0/b.mvt",        //
		"/tiles/3/0/0.png",        // wrong extension
		"/tiles/3/0/0",            // no extension
		"/tiles/3/-1/0.mvt",       // negative
		"/tiles/3/+1/0.mvt",       // sign
		"/tiles/3/99999999/0.mvt", // absurd
		"/tiles/3/0/0.mvt.mvt",    // sloppy suffix
	} {
		wantError(t, hs.do("GET", target), http.StatusBadRequest, CodeInvalidArgument)
	}
}

// -------------------------------------------------------------------- style

const styleTemplate = `{"version":8,"sources":{"omt":{"type":"vector","tiles":["{{PUBLIC_BASE_URL}}/tiles/{z}/{x}/{y}.mvt"]}},` +
	`"glyphs":"{{PUBLIC_BASE_URL}}/tiles/fonts/{fontstack}/{range}.pbf","sprite":"{{PUBLIC_BASE_URL}}/tiles/sprites/osm-liberty","layers":[]}`

func TestStyleUsesPublicBaseURL(t *testing.T) {
	hs := newHarness(t, nil, nil)
	writeFile(t, filepath.Join(hs.static, "style.json"), []byte(styleTemplate))
	rec := hs.do("GET", "/tiles/style.json")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "{{") || !strings.Contains(body, `"https://maps.example.com/tiles/{z}/{x}/{y}.mvt"`) ||
		!strings.Contains(body, `"https://maps.example.com/tiles/fonts/{fontstack}/{range}.pbf"`) ||
		!strings.Contains(body, `"https://maps.example.com/tiles/sprites/osm-liberty"`) {
		t.Errorf("urls not rewritten to the gateway:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || rec.Header().Get("Cache-Control") == "" {
		t.Errorf("caching headers: %v", rec.Header())
	}
	if r := hs.do("GET", "/tiles/style.json", "If-None-Match", etag); r.Code != http.StatusNotModified {
		t.Errorf("conditional style request: %d", r.Code)
	}
}

func TestStyleBaseURLDerivedFromRequestWhenNotConfigured(t *testing.T) {
	hs := newHarness(t, map[string]string{"PUBLIC_BASE_URL": ""}, nil)
	writeFile(t, filepath.Join(hs.static, "style.json"), []byte(styleTemplate))
	get := func(remote string, headers map[string]string, host string) string {
		req := httptest.NewRequest("GET", "/tiles/style.json", nil)
		req.RemoteAddr = remote
		req.Host = host
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	// Direct request: the Host header, plain http.
	if body := get("203.0.113.9:1", nil, "maps.local:8080"); !strings.Contains(body, `"http://maps.local:8080/tiles/`) {
		t.Errorf("direct:\n%s", body)
	}
	// Behind the trusted proxy: forwarded scheme/host are honoured.
	h := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "maps.example.org"}
	if body := get("172.19.0.3:1", h, "gateway:8080"); !strings.Contains(body, `"https://maps.example.org/tiles/`) {
		t.Errorf("proxied:\n%s", body)
	}
	// An untrusted client cannot inject forwarded values.
	if body := get("203.0.113.9:1", h, "maps.local"); strings.Contains(body, "maps.example.org") {
		t.Errorf("untrusted X-Forwarded-* must be ignored:\n%s", body)
	}
	// A hostile Host header cannot break out of the JSON document.
	if body := get("203.0.113.9:1", nil, `evil.example"}],"x":["`); strings.Contains(body, "evil") || !strings.Contains(body, "http://localhost/tiles/") {
		t.Errorf("host sanitising:\n%s", body)
	}
}

func TestStyleAndPreviewMissing(t *testing.T) {
	hs := newHarness(t, nil, nil)
	wantError(t, hs.do("GET", "/tiles/style.json"), http.StatusNotFound, CodeNotFound)
	wantError(t, hs.do("GET", "/tiles/preview.html"), http.StatusNotFound, CodeNotFound)
	writeFile(t, filepath.Join(hs.static, "preview.html"), []byte("<!doctype html><title>map</title>"))
	rec := hs.do("GET", "/tiles/preview.html")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "<title>map</title>") {
		t.Errorf("preview: %d %v", rec.Code, rec.Header())
	}
}

// -------------------------------------------------------------- fonts, sprites

func TestFonts(t *testing.T) {
	hs := newHarness(t, nil, nil)
	writeFile(t, filepath.Join(hs.static, "fonts", "Noto Sans Regular", "0-255.pbf"), []byte("GLYPHS-0-255"))

	rec := hs.do("GET", "/tiles/fonts/Noto%20Sans%20Regular/0-255.pbf")
	if rec.Code != 200 || rec.Body.String() != "GLYPHS-0-255" || rec.Header().Get("Content-Type") != "application/x-protobuf" ||
		rec.Header().Get("ETag") == "" || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("font: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	// A range the stack does not ship is an empty (valid) glyph message, not an error.
	empty := hs.do("GET", "/tiles/fonts/Noto%20Sans%20Regular/65280-65535.pbf")
	if empty.Code != 200 || empty.Body.Len() != 0 {
		t.Errorf("missing range: %d %q", empty.Code, empty.Body)
	}
	// Unknown stacks and hostile names are plain 404s.
	for _, target := range []string{
		"/tiles/fonts/Comic%20Sans/0-255.pbf",
		"/tiles/fonts/%2e%2e/0-255.pbf",
		"/tiles/fonts/Noto%20Sans%20Regular/..%2f..%2f.env",
		"/tiles/fonts/Noto%20Sans%20Regular/0-255.txt",
		"/tiles/fonts/Noto%20Sans%20Regular%2f..%2f..%2fx/0-255.pbf",
	} {
		wantError(t, hs.do("GET", target), http.StatusNotFound, CodeNotFound)
	}
}

func TestSprites(t *testing.T) {
	hs := newHarness(t, nil, nil)
	writeFile(t, filepath.Join(hs.static, "sprites", "osm-liberty.json"), []byte(`{"icon":{}}`))
	writeFile(t, filepath.Join(hs.static, "sprites", "osm-liberty@2x.png"), []byte("\x89PNG"))
	writeFile(t, filepath.Join(hs.static, "secret.json"), []byte(`{"secret":true}`))

	j := hs.do("GET", "/tiles/sprites/osm-liberty.json")
	if j.Code != 200 || !strings.HasPrefix(j.Header().Get("Content-Type"), "application/json") || j.Body.String() != `{"icon":{}}` {
		t.Errorf("json sprite: %d %v", j.Code, j.Header())
	}
	p := hs.do("GET", "/tiles/sprites/osm-liberty@2x.png")
	if p.Code != 200 || p.Header().Get("Content-Type") != "image/png" {
		t.Errorf("png sprite: %d %v", p.Code, p.Header())
	}
	for _, target := range []string{
		"/tiles/sprites/missing.json", "/tiles/sprites/..%2fsecret.json", "/tiles/sprites/osm-liberty.exe", "/tiles/sprites/%2e%2e",
	} {
		wantError(t, hs.do("GET", target), http.StatusNotFound, CodeNotFound)
	}
}

func TestVendorFiles(t *testing.T) {
	hs := newHarness(t, nil, nil)
	writeFile(t, filepath.Join(hs.static, "vendor", "maplibre-gl.js"), []byte("/*js*/"))
	writeFile(t, filepath.Join(hs.static, "vendor", "secret.txt"), []byte("nope"))
	js := hs.do("GET", "/tiles/vendor/maplibre-gl.js")
	if js.Code != 200 || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") || js.Body.String() != "/*js*/" {
		t.Errorf("vendor js: %d %v", js.Code, js.Header())
	}
	// Only the whitelisted files are served; anything else (even existing) is 404.
	for _, target := range []string{"/tiles/vendor/secret.txt", "/tiles/vendor/maplibre-gl.css", "/tiles/vendor/..%2fstyle.json"} {
		wantError(t, hs.do("GET", target), http.StatusNotFound, CodeNotFound)
	}
}

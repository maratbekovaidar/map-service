package stylegen

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// miniLiberty mirrors the constructs of the real OSM Liberty style that
// Transform must handle: the MapTiler vector source, a third-party raster
// source/layer, Roboto fonts, {name}/{name_en}/{ref} text fields and CDN URLs
// for sprite and glyphs.
const miniLiberty = `{
 "version": 8,
 "name": "OSM Liberty",
 "metadata": {"maputnik:license": "https://github.com/maputnik/osm-liberty/blob/gh-pages/LICENSE.md"},
 "sources": {
  "openmaptiles": {"type": "vector", "url": "https://api.maptiler.com/tiles/v3-openmaptiles/tiles.json?key={key}"},
  "natural_earth_shaded_relief": {"type": "raster", "tiles": ["https://klokantech.github.io/naturalearthtiles/tiles/{z}/{x}/{y}.png"], "tileSize": 256}
 },
 "sprite": "https://maputnik.github.io/osm-liberty/sprites/osm-liberty",
 "glyphs": "https://orangemug.github.io/font-glyphs/glyphs/{fontstack}/{range}.pbf",
 "layers": [
  {"id": "background", "type": "background"},
  {"id": "natural_earth", "type": "raster", "source": "natural_earth_shaded_relief"},
  {"id": "water", "type": "fill", "source": "openmaptiles", "source-layer": "water"},
  {"id": "place_city", "type": "symbol", "source": "openmaptiles", "source-layer": "place",
   "layout": {"text-field": "{name_en}", "text-font": ["Roboto Medium"]}},
  {"id": "poi", "type": "symbol", "source": "openmaptiles", "source-layer": "poi",
   "layout": {"text-field": "{name}", "text-font": ["Roboto Regular"]}},
  {"id": "waterway_name", "type": "symbol", "source": "openmaptiles", "source-layer": "waterway",
   "layout": {"text-field": "{name}", "text-font": ["Roboto Condensed Italic"]}},
  {"id": "road_shield", "type": "symbol", "source": "openmaptiles", "source-layer": "transportation_name",
   "layout": {"text-field": "{ref}", "text-font": ["Roboto Regular"]}}
 ]
}`

func testOptions() Options {
	return Options{
		ID:          "liberty",
		Name:        "OSM Liberty",
		Description: "test",
		DerivedFrom: "OSM Liberty",
		Sprite:      "osm-liberty",
		Languages:   []string{"ru", "kk", "en"},
		TileMaxZoom: 14,
		Center:      [2]float64{71.4306, 51.1283},
		Zoom:        12,
		FontMap: map[string]string{
			"Roboto Regular": "Noto Sans Regular", "Roboto Medium": "Noto Sans Bold", "Roboto Condensed Italic": "Noto Sans Italic",
		},
	}
}

func transform(t *testing.T, in string, opt Options) map[string]any {
	t.Helper()
	out, err := Transform([]byte(in), opt)
	if err != nil {
		t.Fatal(err)
	}
	var style map[string]any
	if err := json.Unmarshal(out, &style); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return style
}

func TestTransformRewiresEverythingToTheGateway(t *testing.T) {
	style := transform(t, miniLiberty, testOptions())

	if style["sprite"] != "{{PUBLIC_BASE_URL}}/tiles/sprites/osm-liberty" ||
		style["glyphs"] != "{{PUBLIC_BASE_URL}}/tiles/fonts/{fontstack}/{range}.pbf" {
		t.Errorf("sprite/glyphs: %v / %v", style["sprite"], style["glyphs"])
	}
	sources := style["sources"].(map[string]any)
	if len(sources) != 1 {
		t.Fatalf("only the vector source may remain, got %v", sources)
	}
	omt := sources["openmaptiles"].(map[string]any)
	tilesList := omt["tiles"].([]any)
	if omt["type"] != "vector" || tilesList[0] != "{{PUBLIC_BASE_URL}}/tiles/{z}/{x}/{y}.mvt" || omt["url"] != nil {
		t.Errorf("vector source: %v", omt)
	}
	if omt["maxzoom"].(float64) != 14 {
		t.Errorf("maxzoom must match the archive so clients overzoom: %v", omt["maxzoom"])
	}
	attr := omt["attribution"].(string)
	if !strings.Contains(attr, "OpenStreetMap contributors") || !strings.Contains(attr, "OpenMapTiles") {
		t.Errorf("attribution: %q", attr)
	}
	if c := style["center"].([]any); c[0] != 71.4306 || c[1] != 51.1283 || style["zoom"].(float64) != 12 {
		t.Errorf("camera: %v %v", style["center"], style["zoom"])
	}
}

func TestTransformDropsThirdPartyLayersAndRenamesFonts(t *testing.T) {
	style := transform(t, miniLiberty, testOptions())
	layers := style["layers"].([]any)
	ids := []string{}
	for _, l := range layers {
		ids = append(ids, l.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, ",") != "background,water,place_city,poi,waterway_name,road_shield" {
		t.Errorf("layers: %v", ids)
	}
	fontOf := func(id string) string {
		for _, l := range layers {
			m := l.(map[string]any)
			if m["id"] == id {
				return m["layout"].(map[string]any)["text-font"].([]any)[0].(string)
			}
		}
		return ""
	}
	if fontOf("place_city") != "Noto Sans Bold" || fontOf("poi") != "Noto Sans Regular" || fontOf("waterway_name") != "Noto Sans Italic" {
		t.Error("fonts not renamed")
	}
}

func TestTransformLabelsUseTheFirstLanguageWithFallback(t *testing.T) {
	style := transform(t, miniLiberty, testOptions())
	want := `["coalesce",["get","name:ru"],["get","name:latin"]]`
	for _, l := range style["layers"].([]any) {
		m := l.(map[string]any)
		layout, _ := m["layout"].(map[string]any)
		if layout == nil {
			continue
		}
		tf := layout["text-field"]
		switch m["id"] {
		case "place_city", "poi", "waterway_name":
			b, _ := json.Marshal(tf)
			if string(b) != want {
				t.Errorf("%v text-field = %s, want %s", m["id"], b, want)
			}
		case "road_shield":
			if tf != "{ref}" {
				t.Errorf("language-neutral {ref} must be kept: %v", tf)
			}
		}
	}
	kkOpt := testOptions()
	kkOpt.Languages = []string{"kk", "ru"}
	kk := transform(t, miniLiberty, kkOpt)
	b, _ := json.Marshal(kk["layers"].([]any)[2].(map[string]any)["layout"].(map[string]any)["text-field"])
	if string(b) != `["coalesce",["get","name:kk"],["get","name:latin"]]` {
		t.Errorf("kk labels: %s", b)
	}
}

func TestTransformGuardsAgainstUpstreamChanges(t *testing.T) {
	opt := testOptions()
	mk := func(layer string) string {
		return strings.Replace(miniLiberty, `{"id": "background", "type": "background"},`,
			`{"id": "background", "type": "background"},`+layer+`,`, 1)
	}
	cases := map[string]string{
		"unmapped font":           mk(`{"id":"x","type":"symbol","source":"openmaptiles","layout":{"text-font":["Comic Sans"]}}`),
		"unknown text field":      mk(`{"id":"x","type":"symbol","source":"openmaptiles","layout":{"text-field":"{class}"}}`),
		"external url in a layer": mk(`{"id":"x","type":"fill","source":"openmaptiles","paint":{"fill-pattern":"https://cdn.example.com/p.png"}}`),
	}
	for name, in := range cases {
		if _, err := Transform([]byte(in), opt); err == nil {
			t.Errorf("%s: Transform must fail loudly", name)
		}
	}
	if _, err := Transform([]byte(`{"sources":{}}`), opt); err == nil {
		t.Error("a style without the openmaptiles source must be rejected")
	}
	if _, err := Transform([]byte(`not json`), opt); err == nil {
		t.Error("invalid JSON must be rejected")
	}
	if _, err := Transform([]byte(miniLiberty), Options{ID: "x", FontMap: opt.FontMap}); err == nil {
		t.Error("languages are required")
	}
	bad := testOptions()
	bad.ID = "../x"
	if _, err := Transform([]byte(miniLiberty), bad); err == nil {
		t.Error("the style id becomes a file name and must be validated")
	}
	// The licence link in metadata is data, not a fetched URL.
	if _, err := Transform([]byte(miniLiberty), opt); err != nil {
		t.Errorf("metadata URLs are allowed: %v", err)
	}
}

func makeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	zw.Close()
	p := filepath.Join(t.TempDir(), "glyphs.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractGlyphs(t *testing.T) {
	zp := makeZip(t, map[string]string{
		"Noto Sans Regular/0-255.pbf":       "reg-0",
		"Noto Sans Regular/1024-1279.pbf":   "reg-cyr",
		"Noto Sans Regular/65280-65535.pbf": "reg-last",
		"Noto Sans Bold/0-255.pbf":          "bold-0",
		"Noto Sans Bold/1024-1279.pbf":      "bold-cyr",
		"Noto Sans Thai/0-255.pbf":          "thai",
	})
	out := t.TempDir()
	n, err := ExtractGlyphs(zp, out, GlyphSpec{Stacks: []string{"Noto Sans Regular", "Noto Sans Bold"}, Ranges: []string{"0-255", "1024-1279"}})
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "Noto Sans Bold", "1024-1279.pbf"))
	if string(got) != "bold-cyr" {
		t.Errorf("content: %q", got)
	}
	for _, unwanted := range []string{"Noto Sans Regular/65280-65535.pbf", "Noto Sans Thai/0-255.pbf"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(unwanted))); err == nil {
			t.Errorf("%s must not be extracted", unwanted)
		}
	}
	// A range that the archive lacks is an error, not a silent gap.
	if _, err := ExtractGlyphs(zp, t.TempDir(), GlyphSpec{Stacks: []string{"Noto Sans Regular"}, Ranges: []string{"7680-7935"}}); err == nil {
		t.Error("missing range must fail")
	}
	if _, err := ExtractGlyphs(filepath.Join(t.TempDir(), "nope.zip"), out, GlyphSpec{}); err == nil {
		t.Error("missing archive must fail")
	}
}

func TestKazakhLettersAreInTheDefaultRanges(t *testing.T) {
	covered := func(r rune) bool {
		for _, rg := range DefaultRanges {
			var lo, hi int
			if _, err := fmtSscanRange(rg, &lo, &hi); err == nil && int(r) >= lo && int(r) <= hi {
				return true
			}
		}
		return false
	}
	for _, r := range "ӘәҒғҚқҢңӨөҰұҮүҺһІіЁёЙй₸ʼ" {
		if !covered(r) {
			t.Errorf("U+%04X %q is not covered by DefaultRanges", r, string(r))
		}
	}
}

func fmtSscanRange(s string, lo, hi *int) (int, error) {
	i := strings.IndexByte(s, '-')
	if i < 0 {
		return 0, os.ErrInvalid
	}
	var err error
	if *lo, err = atoi(s[:i]); err != nil {
		return 0, err
	}
	*hi, err = atoi(s[i+1:])
	return 2, err
}

func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func TestFetchVerifiesAndCaches(t *testing.T) {
	body := []byte("pinned content")
	sum := sha256.Sum256(body)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/bad" {
			_, _ = w.Write([]byte("tampered"))
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	cache := t.TempDir()
	good := Source{Name: "f.bin", URL: srv.URL + "/good", SHA256: hex.EncodeToString(sum[:])}

	p, err := Fetch(context.Background(), good, cache, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
		t.Errorf("content: %q", b)
	}
	if _, err := Fetch(context.Background(), good, cache, srv.Client()); err != nil || hits != 1 {
		t.Errorf("a verified cached copy must not be downloaded again (hits=%d, err=%v)", hits, err)
	}

	bad := Source{Name: "g.bin", URL: srv.URL + "/bad", SHA256: good.SHA256}
	if _, err := Fetch(context.Background(), bad, cache, srv.Client()); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("tampered download must be rejected: %v", err)
	}
	for _, f := range []string{"g.bin", "g.bin.part"} {
		if _, err := os.Stat(filepath.Join(cache, f)); err == nil {
			t.Errorf("%s must not remain after a failed verification", f)
		}
	}
	if _, err := Fetch(context.Background(), Source{Name: "h", URL: srv.URL + "/x", SHA256: good.SHA256}, cache, srv.Client()); err != nil {
		t.Log("(server answers any path with 200, as designed)")
	}
}

func TestPinsAreRealHashes(t *testing.T) {
	all := []Source{NotoGlyphs(), NotoLicense()}
	for _, b := range BaseStyles() {
		all = append(all, b.Sources()...)
	}
	all = append(all, MapLibreFiles()...)
	all = append(all, SwaggerUIFiles()...)
	all = append(all, SwaggerUILicenses()...)
	for _, s := range all {
		if len(s.SHA256) != 64 || strings.Trim(s.SHA256, "0123456789abcdef") != "" {
			t.Errorf("%s: SHA256 %q is not a pinned hash", s.Name, s.SHA256)
		}
		if !strings.HasPrefix(s.URL, "https://") {
			t.Errorf("%s: URL must be https: %s", s.Name, s.URL)
		}
	}
}

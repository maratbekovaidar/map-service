package stylegen

import (
	"encoding/json"
	"strings"
	"testing"
)

// miniCarto mirrors the constructs of Positron / Dark Matter: Metropolis font
// stacks with a Noto fallback, two-line latin/non-latin labels as tokens and
// as expressions, and ref expressions.
const miniCarto = `{
 "version": 8,
 "name": "Positron",
 "sources": {"openmaptiles": {"type": "vector", "url": "https://api.maptiler.com/tiles/v3-openmaptiles/tiles.json?key={key}"}},
 "sprite": "https://openmaptiles.github.io/positron-gl-style/sprite",
 "glyphs": "https://api.maptiler.com/fonts/{fontstack}/{range}.pbf?key={key}",
 "layers": [
  {"id": "background", "type": "background"},
  {"id": "place_city", "type": "symbol", "source": "openmaptiles", "source-layer": "place",
   "layout": {"text-field": ["concat", ["get", "name:latin"], "\n", ["get", "name:nonlatin"]], "text-font": ["Metropolis Regular", "Noto Sans Regular"]}},
  {"id": "place_town", "type": "symbol", "source": "openmaptiles", "source-layer": "place",
   "layout": {"text-field": "{name:latin}\n{name:nonlatin}", "text-font": ["Metropolis Light Italic", "Noto Sans Italic"]}},
  {"id": "highway_name_motorway", "type": "symbol", "source": "openmaptiles", "source-layer": "transportation_name",
   "layout": {"text-field": ["to-string", ["get", "ref"]], "text-font": ["Metropolis Light", "Noto Sans Regular"]}}
 ]
}`

func cartoOptions() Options {
	opt := testOptions()
	opt.ID, opt.Name, opt.Sprite = "positron", "Positron", "positron"
	opt.FontMap = metropolis
	opt.ExtraAttribution = cartoAttribution
	return opt
}

func layerByID(t *testing.T, style map[string]any, id string) map[string]any {
	t.Helper()
	for _, l := range style["layers"].([]any) {
		if m := l.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no layer %q", id)
	return nil
}

func TestTransformHandlesCartoStyles(t *testing.T) {
	style := transform(t, miniCarto, cartoOptions())
	if style["sprite"] != "{{PUBLIC_BASE_URL}}/tiles/sprites/positron" || style["name"] != "Positron" {
		t.Errorf("sprite/name: %v %v", style["sprite"], style["name"])
	}
	meta := style["metadata"].(map[string]any)
	if meta[MetaStyleID] != "positron" || meta[MetaDescription] != "test" {
		t.Errorf("metadata: %v", meta)
	}
	attr := style["sources"].(map[string]any)["openmaptiles"].(map[string]any)["attribution"].(string)
	if !strings.Contains(attr, "OpenStreetMap") || !strings.Contains(attr, "CARTO") {
		t.Errorf("attribution must credit OSM, OpenMapTiles and CARTO: %q", attr)
	}
	want := `["coalesce",["get","name:ru"],["get","name:latin"]]`
	for _, id := range []string{"place_city", "place_town"} {
		layout := layerByID(t, style, id)["layout"].(map[string]any)
		if b, _ := json.Marshal(layout["text-field"]); string(b) != want {
			t.Errorf("%s text-field = %s, want %s", id, b, want)
		}
	}
	// Fallback stacks collapse to one installed font (no "A,A" glyph URLs).
	fonts := func(id string) string {
		b, _ := json.Marshal(layerByID(t, style, id)["layout"].(map[string]any)["text-font"])
		return string(b)
	}
	if fonts("place_city") != `["Noto Sans Regular"]` || fonts("place_town") != `["Noto Sans Italic"]` {
		t.Errorf("fonts: %s %s", fonts("place_city"), fonts("place_town"))
	}
	if b, _ := json.Marshal(layerByID(t, style, "highway_name_motorway")["layout"].(map[string]any)["text-field"]); string(b) != `["to-string",["get","ref"]]` {
		t.Errorf("ref expression must be kept: %s", b)
	}
}

func TestTransformWithoutSprite(t *testing.T) {
	opt := testOptions()
	opt.Sprite = ""
	if style := transform(t, miniLiberty, opt); style["sprite"] != nil {
		t.Errorf("sprite must be removed when the style has none: %v", style["sprite"])
	}
}

func TestCheckFonts(t *testing.T) {
	style, err := Transform([]byte(miniCarto), cartoOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFonts(style, []string{"Noto Sans Regular", "Noto Sans Italic"}); err != nil {
		t.Errorf("installed fonts: %v", err)
	}
	if err := CheckFonts(style, []string{"Noto Sans Regular"}); err == nil || !strings.Contains(err.Error(), "Noto Sans Italic") {
		t.Errorf("a font without glyphs must be reported: %v", err)
	}
	if err := CheckFonts([]byte(`{"layers":[{"id":"x","layout":{"text-font":["step",["zoom"],"a",1,"b"]}}]}`), []string{"a"}); err == nil {
		t.Error("expression font stacks cannot be checked and must be rejected")
	}
}

func baseForThemes(t *testing.T) []byte {
	t.Helper()
	b, err := Transform([]byte(miniLiberty), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApplyTheme(t *testing.T) {
	th, err := ParseTheme([]byte(`{
	  "id": "warm", "base": "liberty", "name": "Warm", "description": "d", "derived-from": "own",
	  "layers": {
	    "background": {"paint": {"background-color": "#F1EEE0"}},
	    "poi": {"note": "n", "minzoom": 15, "layout": {"text-font": ["Noto Sans Bold"], "text-field": "{name}"}, "paint": {"text-color": "#123"}},
	    "water": {"paint": {"fill-color": "#9ED3F0", "fill-opacity": null}}
	  },
	  "insert": [
	    {"before": "place_city", "layer": {"id": "housenumber", "type": "symbol", "source": "openmaptiles", "source-layer": "housenumber",
	      "layout": {"text-field": ["get", "housenumber"]}}},
	    {"before": "road_shield", "layer": {"id": "labels", "type": "symbol", "source": "openmaptiles", "source-layer": "place",
	      "layout": {"text-field": "{name}"}}}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	base := baseForThemes(t)
	// water starts with an opacity that the theme removes
	base = []byte(strings.Replace(string(base), `"source-layer": "water"`, `"source-layer": "water", "paint": {"fill-opacity": 0.5}`, 1))
	out, err := ApplyTheme(base, th, "kk")
	if err != nil {
		t.Fatal(err)
	}
	var style map[string]any
	if err := json.Unmarshal(out, &style); err != nil {
		t.Fatal(err)
	}
	meta := style["metadata"].(map[string]any)
	if style["name"] != "Warm" || meta[MetaStyleID] != "warm" || meta[MetaDescription] != "d" ||
		!strings.HasPrefix(meta[MetaDerivedFrom].(string), "own; ") || meta[MetaLanguages] == nil {
		t.Errorf("identity/metadata: %v %v", style["name"], meta)
	}
	if layerByID(t, style, "background")["paint"].(map[string]any)["background-color"] != "#F1EEE0" {
		t.Error("paint override not applied")
	}
	poi := layerByID(t, style, "poi")
	if poi["minzoom"].(float64) != 15 || poi["paint"].(map[string]any)["text-color"] != "#123" {
		t.Errorf("poi patch: %v", poi)
	}
	if b, _ := json.Marshal(poi["layout"].(map[string]any)["text-field"]); string(b) != `["coalesce",["get","name:kk"],["get","name:latin"]]` {
		t.Errorf("{name} in a theme must become the language expression: %s", b)
	}
	if paint := layerByID(t, style, "water")["paint"]; paint == nil || paint.(map[string]any)["fill-opacity"] != nil {
		t.Errorf("null must delete a property: %v", paint)
	}
	var ids []string
	for _, l := range style["layers"].([]any) {
		ids = append(ids, l.(map[string]any)["id"].(string))
	}
	if got := strings.Join(ids, ","); got != "background,water,housenumber,place_city,poi,waterway_name,labels,road_shield" {
		t.Errorf("layer order: %s", got)
	}
	if tf := layerByID(t, style, "labels")["layout"].(map[string]any)["text-field"]; tf == "{name}" {
		t.Error("inserted {name} labels must be language-aware")
	}
}

func TestApplyThemeFailsLoudly(t *testing.T) {
	base := baseForThemes(t)
	for name, js := range map[string]string{
		"unknown layer":       `{"id":"t","base":"liberty","name":"T","layers":{"no_such_layer":{"paint":{"x":1}}}}`,
		"missing anchor":      `{"id":"t","base":"liberty","name":"T","insert":[{"before":"nope","layer":{"id":"n","type":"fill"}}]}`,
		"duplicate insert id": `{"id":"t","base":"liberty","name":"T","insert":[{"before":"water","layer":{"id":"poi","type":"fill"}}]}`,
	} {
		th, err := ParseTheme([]byte(js))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := ApplyTheme(base, th, "ru"); err == nil {
			t.Errorf("%s: ApplyTheme must fail", name)
		}
	}
	for name, js := range map[string]string{
		"typo in a key": `{"id":"t","base":"liberty","name":"T","layer":{}}`,
		"bad id":        `{"id":"Bad Id","base":"liberty","name":"T"}`,
		"no base":       `{"id":"t","name":"T"}`,
	} {
		if _, err := ParseTheme([]byte(js)); err == nil {
			t.Errorf("%s: ParseTheme must fail", name)
		}
	}
}

func TestEmbeddedThemesParse(t *testing.T) {
	themes, err := Themes()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, b := range BaseStyles() {
		ids[b.ID] = true
	}
	if len(themes) == 0 {
		t.Fatal("no embedded themes")
	}
	for _, th := range themes {
		if !ids[th.Base] {
			t.Errorf("theme %s: base %q is not a base style", th.ID, th.Base)
		}
		if ids[th.ID] {
			t.Errorf("theme %s: id collides with a base style", th.ID)
		}
		ids[th.ID] = true
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"liberty", "2gis-like", "dark-matter", "a"} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "-x", "Upper", "a_b", "a/b", "..", "a.json", strings.Repeat("a", 49)} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

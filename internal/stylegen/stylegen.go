// Package stylegen builds the map style assets served by the gateway from
// pinned upstream sources: the base styles (OSM Liberty, Positron, Dark
// Matter, rewired to this service's tiles, glyphs and sprites), the themes
// derived from them (themes/*.json), Noto Sans glyph ranges and the sprite
// sheets.
//
// The transformations are pure functions so they can be unit-tested; the
// network side lives in fetch.go.
package stylegen

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"map-service/internal/styles"
)

// PlaceholderBaseURL is replaced by the gateway with PUBLIC_BASE_URL at request time.
const PlaceholderBaseURL = "{{PUBLIC_BASE_URL}}"

// Attribution is shown by the map. OpenStreetMap's ODbL and the OpenMapTiles
// licence (OSM Liberty is built on it) both require visible credit.
const Attribution = `<a href="https://www.openstreetmap.org/copyright" target="_blank">&copy; OpenStreetMap contributors</a> ` +
	`<a href="https://openmaptiles.org/" target="_blank">&copy; OpenMapTiles</a>`

// Metadata keys written into every generated style. The gateway's style
// catalogue (GET /v1/styles) reads the name and these.
const (
	MetaStyleID     = "map-service:style-id"
	MetaDescription = "map-service:description"
	MetaDerivedFrom = "map-service:derived-from"
	MetaLanguages   = "map-service:languages"
)

// Options controls Transform.
type Options struct {
	// ID, Name and Description identify the style in the catalogue;
	// DerivedFrom is the credit of the upstream design and code.
	ID          string
	Name        string
	Description string
	DerivedFrom string
	// Sprite is the sprite sheet name under /tiles/sprites/ ("" = none).
	Sprite string
	// ExtraAttribution is appended to Attribution (design credit).
	ExtraAttribution string
	// Languages in priority order (LANGUAGES); labels use the first one and
	// fall back to the default OSM name.
	Languages []string
	// FontMap renames fonts of the upstream style (Roboto -> Noto Sans).
	FontMap map[string]string
	// TileMaxZoom is the highest zoom stored in the PMTiles archive.
	TileMaxZoom int
	// Center ([lon, lat]) and Zoom are the initial camera of the style.
	Center [2]float64
	Zoom   float64
}

// NameExpression returns the MapLibre expression for a label in lang: the
// translation when the tile carries one, else the default (local) name.
// tilemaker writes the default name as "name:latin" and a translation as
// "name:<lang>" only when it differs from the default.
func NameExpression(lang string) []any {
	return []any{"coalesce", []any{"get", "name:" + lang}, []any{"get", "name:latin"}}
}

// Transform rewires an upstream OpenMapTiles-schema style.json (OSM Liberty,
// Positron, Dark Matter) to this service.
func Transform(raw []byte, opt Options) ([]byte, error) {
	if len(opt.Languages) == 0 {
		return nil, fmt.Errorf("stylegen: at least one language is required")
	}
	if !ValidID(opt.ID) {
		return nil, fmt.Errorf("stylegen: invalid style id %q", opt.ID)
	}
	var style map[string]any
	if err := json.Unmarshal(raw, &style); err != nil {
		return nil, fmt.Errorf("stylegen: parse style: %w", err)
	}

	style["name"] = opt.Name
	style["metadata"] = map[string]any{
		MetaStyleID:     opt.ID,
		MetaDescription: opt.Description,
		MetaDerivedFrom: opt.DerivedFrom,
		MetaLanguages:   opt.Languages,
	}
	style["center"] = []any{opt.Center[0], opt.Center[1]}
	style["zoom"] = opt.Zoom
	if opt.Sprite != "" {
		style["sprite"] = PlaceholderBaseURL + "/tiles/sprites/" + opt.Sprite
	} else {
		delete(style, "sprite")
	}
	style["glyphs"] = PlaceholderBaseURL + "/tiles/fonts/{fontstack}/{range}.pbf"

	// Sources: keep only the vector source, pointed at the gateway.
	sources, _ := style["sources"].(map[string]any)
	if sources == nil || sources["openmaptiles"] == nil {
		return nil, fmt.Errorf("stylegen: upstream style has no 'openmaptiles' source")
	}
	attribution := Attribution
	if opt.ExtraAttribution != "" {
		attribution += " " + opt.ExtraAttribution
	}
	removed := map[string]bool{}
	for name := range sources {
		if name != "openmaptiles" {
			removed[name] = true
		}
	}
	style["sources"] = map[string]any{
		"openmaptiles": map[string]any{
			"type":        "vector",
			"tiles":       []any{PlaceholderBaseURL + "/tiles/{z}/{x}/{y}.mvt"},
			"minzoom":     0,
			"maxzoom":     opt.TileMaxZoom,
			"attribution": attribution,
		},
	}

	layers, _ := style["layers"].([]any)
	kept := make([]any, 0, len(layers))
	for _, l := range layers {
		layer, ok := l.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("stylegen: layer is not an object")
		}
		if src, _ := layer["source"].(string); removed[src] {
			continue // e.g. the Natural Earth raster hosted on a third-party CDN
		}
		if err := rewriteLayer(layer, opt); err != nil {
			return nil, fmt.Errorf("stylegen: layer %v: %w", layer["id"], err)
		}
		kept = append(kept, layer)
	}
	style["layers"] = kept

	return encodeStyle(style)
}

func encodeStyle(style map[string]any) ([]byte, error) {
	if err := checkNoExternalURLs(style); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(style, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ValidID reports whether id can name a style (the gateway's rule).
func ValidID(id string) bool { return styles.ValidID(id) }

func rewriteLayer(layer map[string]any, opt Options) error {
	layout, _ := layer["layout"].(map[string]any)
	if layout == nil {
		return nil
	}
	if fonts, ok := layout["text-font"].([]any); ok {
		// Upstream stacks like ["Metropolis Regular", "Noto Sans Regular"]
		// collapse to one font: a duplicate would make the glyph URL a
		// comma-separated stack the gateway does not serve.
		out := make([]any, 0, len(fonts))
		seen := map[string]bool{}
		for _, f := range fonts {
			name, _ := f.(string)
			to, ok := opt.FontMap[name]
			if !ok {
				return fmt.Errorf("font %q has no replacement in FontMap", name)
			}
			if !seen[to] {
				seen[to] = true
				out = append(out, to)
			}
		}
		layout["text-font"] = out
	}
	if tf, ok := layout["text-field"]; ok {
		b, _ := json.Marshal(tf)
		switch s := string(b); {
		case strings.Contains(s, "name"):
			// {name}, {name_en}, "{name:latin}\n{name:nonlatin}" and the
			// equivalent expressions all become one label in the chosen
			// language, so the client can switch it (see preview.html).
			layout["text-field"] = NameExpression(opt.Languages[0])
		case strings.Contains(s, "ref"):
			// road reference numbers are language-neutral
		default:
			return fmt.Errorf("unhandled text-field %s", s)
		}
	}
	return nil
}

// CheckFonts fails if a layer of the style uses a font stack that is not in
// stacks (the glyph stacks installed under fonts/).
func CheckFonts(raw []byte, stacks []string) error {
	var style struct {
		Layers []struct {
			ID     string         `json:"id"`
			Layout map[string]any `json:"layout"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &style); err != nil {
		return fmt.Errorf("stylegen: parse style: %w", err)
	}
	have := map[string]bool{}
	for _, s := range stacks {
		have[s] = true
	}
	for _, l := range style.Layers {
		v, ok := l.Layout["text-font"]
		if !ok {
			continue
		}
		fonts, _ := v.([]any)
		if len(fonts) == 0 {
			return fmt.Errorf("stylegen: layer %s: text-font must be a list of font names, got %v", l.ID, v)
		}
		for _, f := range fonts {
			if name, _ := f.(string); !have[name] {
				return fmt.Errorf("stylegen: layer %s uses font %v, which has no glyphs (have %v)", l.ID, f, stacks)
			}
		}
	}
	return nil
}

// checkNoExternalURLs fails if any string outside "metadata" is an absolute
// http(s) URL: after Transform the style may only reference this service
// through the placeholder, never a third-party host.
func checkNoExternalURLs(v any) error {
	var walk func(path string, v any) error
	walk = func(p string, v any) error {
		switch t := v.(type) {
		case string:
			if (strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")) && !strings.Contains(t, "openstreetmap.org/copyright") {
				return fmt.Errorf("stylegen: external URL left in the style at %s: %s", p, t)
			}
		case []any:
			for i, e := range t {
				if err := walk(fmt.Sprintf("%s[%d]", p, i), e); err != nil {
					return err
				}
			}
		case map[string]any:
			for k, e := range t {
				if p == "" && k == "metadata" {
					continue
				}
				if err := walk(p+"."+k, e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("", v)
}

// GlyphSpec selects what ExtractGlyphs copies out of the glyph archive.
type GlyphSpec struct {
	Stacks []string // font stack directory names, e.g. "Noto Sans Regular"
	Ranges []string // "0-255", "1024-1279", ...
}

// DefaultRanges covers Latin, Latin Extended, IPA/modifier letters (the
// Kazakh Latin apostrophe U+02BC), combining marks, Greek, Cyrillic (with the
// Kazakh letters Ә Ғ Қ Ң Ө Ұ Ү Һ І, all in U+0400-04FF), Cyrillic Supplement,
// Latin Extended Additional, general punctuation and letterlike/currency
// symbols (₸ U+20B8 is in the 8192-8447 range).
var DefaultRanges = []string{
	"0-255", "256-511", "512-767", "768-1023", "1024-1279", "1280-1535",
	"7680-7935", "8192-8447", "8448-8703",
}

// ExtractGlyphs copies the selected stacks/ranges from the zip archive at
// zipPath into outDir/<stack>/<range>.pbf. Missing files are an error: the
// tool must fail loudly if the upstream archive layout changes.
func ExtractGlyphs(zipPath, outDir string, spec GlyphSpec) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, fmt.Errorf("stylegen: open glyph archive: %w", err)
	}
	defer zr.Close()
	index := map[string]*zip.File{}
	for _, f := range zr.File {
		index[path.Clean(f.Name)] = f
	}
	var want []string
	for _, st := range spec.Stacks {
		for _, r := range spec.Ranges {
			want = append(want, st+"/"+r+".pbf")
		}
	}
	sort.Strings(want)
	n := 0
	for _, name := range want {
		f := index[name]
		if f == nil {
			return n, fmt.Errorf("stylegen: %q not found in the glyph archive", name)
		}
		if err := copyZipFile(f, filepath.Join(outDir, filepath.FromSlash(name))); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func copyZipFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, 8<<20)); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

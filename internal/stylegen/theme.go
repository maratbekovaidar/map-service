package stylegen

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
)

// A theme restyles one of the generated base styles without touching its
// data wiring: it overrides paint/layout properties of existing layers and
// can insert new layers. Themes live in themes/*.json (embedded below) so a
// colour can be tweaked without writing Go; `make style-assets` rebuilds
// deploy/tiles/styles/<id>.json from them.
//
// Every layer id a theme names must exist in the base style, so an upstream
// change of the base fails the build instead of silently dropping the theme.

//go:embed themes/*.json
var themeFiles embed.FS

// Theme is the schema of themes/*.json.
type Theme struct {
	ID          string   `json:"id"`
	Base        string   `json:"base"` // id of a BaseStyle
	Name        string   `json:"name"`
	Description string   `json:"description"`
	DerivedFrom string   `json:"derived-from"`
	Notes       []string `json:"notes,omitempty"` // free text for humans
	// Layers patches existing layers by id.
	Layers map[string]LayerPatch `json:"layers"`
	// Insert adds layers, each before an existing layer.
	Insert []Insertion `json:"insert,omitempty"`
}

// LayerPatch overrides parts of one layer. A property set to null in Paint
// or Layout is removed (the renderer default applies).
type LayerPatch struct {
	Note    string         `json:"note,omitempty"` // free text for humans
	Paint   map[string]any `json:"paint,omitempty"`
	Layout  map[string]any `json:"layout,omitempty"`
	Filter  any            `json:"filter,omitempty"`
	MinZoom *float64       `json:"minzoom,omitempty"`
	MaxZoom *float64       `json:"maxzoom,omitempty"`
}

// Insertion is a new layer placed before the layer with id Before. A
// "text-field" of "{name}" is replaced by the language-aware expression.
type Insertion struct {
	Note   string         `json:"note,omitempty"`
	Before string         `json:"before"`
	Layer  map[string]any `json:"layer"`
}

// Themes returns the embedded themes sorted by id.
func Themes() ([]Theme, error) {
	names, err := themeFiles.ReadDir("themes")
	if err != nil {
		return nil, err
	}
	var out []Theme
	for _, e := range names {
		b, err := themeFiles.ReadFile(path.Join("themes", e.Name()))
		if err != nil {
			return nil, err
		}
		th, err := ParseTheme(b)
		if err != nil {
			return nil, fmt.Errorf("stylegen: themes/%s: %w", e.Name(), err)
		}
		out = append(out, th)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ParseTheme decodes a theme strictly: an unknown key is a typo, not a no-op.
func ParseTheme(b []byte) (Theme, error) {
	var th Theme
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&th); err != nil {
		return th, err
	}
	switch {
	case !ValidID(th.ID):
		return th, fmt.Errorf("invalid id %q", th.ID)
	case th.Base == "" || th.Name == "":
		return th, fmt.Errorf("base and name are required")
	}
	return th, nil
}

// ApplyTheme builds the themed style from base, a style produced by
// Transform. lang is the label language ("{name}" of inserted layers).
func ApplyTheme(base []byte, th Theme, lang string) ([]byte, error) {
	var style map[string]any
	if err := json.Unmarshal(base, &style); err != nil {
		return nil, fmt.Errorf("stylegen: parse base style: %w", err)
	}
	meta, _ := style["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	derived := th.DerivedFrom
	if up, _ := meta[MetaDerivedFrom].(string); up != "" {
		derived += "; layers and data wiring: " + up
	}
	meta[MetaStyleID] = th.ID
	meta[MetaDescription] = th.Description
	meta[MetaDerivedFrom] = derived
	style["metadata"] = meta
	style["name"] = th.Name

	layers, _ := style["layers"].([]any)
	byID := map[string]map[string]any{}
	for _, l := range layers {
		m, _ := l.(map[string]any)
		id, _ := m["id"].(string)
		byID[id] = m
	}
	for _, id := range sortedKeys(th.Layers) {
		layer, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("stylegen: theme %s: base style %s has no layer %q", th.ID, th.Base, id)
		}
		p := th.Layers[id]
		mergeProps(layer, "paint", p.Paint)
		mergeProps(layer, "layout", p.Layout)
		if p.Filter != nil {
			layer["filter"] = p.Filter
		}
		if p.MinZoom != nil {
			layer["minzoom"] = *p.MinZoom
		}
		if p.MaxZoom != nil {
			layer["maxzoom"] = *p.MaxZoom
		}
		nameField(layer, lang)
	}
	for _, ins := range th.Insert {
		id, _ := ins.Layer["id"].(string)
		if id == "" || byID[id] != nil {
			return nil, fmt.Errorf("stylegen: theme %s: inserted layer needs a new unique id, got %q", th.ID, id)
		}
		at := -1
		for i, l := range layers {
			if l.(map[string]any)["id"] == ins.Before {
				at = i
				break
			}
		}
		if at < 0 {
			return nil, fmt.Errorf("stylegen: theme %s: layer %q: no layer %q to insert before", th.ID, id, ins.Before)
		}
		layer := ins.Layer
		nameField(layer, lang)
		layers = append(layers[:at], append([]any{layer}, layers[at:]...)...)
		byID[id] = layer
	}
	style["layers"] = layers
	return encodeStyle(style)
}

// mergeProps sets props on layer[key]; a nil value deletes the property.
func mergeProps(layer map[string]any, key string, props map[string]any) {
	if len(props) == 0 {
		return
	}
	dst, _ := layer[key].(map[string]any)
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range props {
		if v == nil {
			delete(dst, k)
		} else {
			dst[k] = v
		}
	}
	if len(dst) == 0 {
		delete(layer, key)
	} else {
		layer[key] = dst
	}
}

func nameField(layer map[string]any, lang string) {
	if layout, _ := layer["layout"].(map[string]any); layout != nil && layout["text-field"] == "{name}" {
		layout["text-field"] = NameExpression(lang)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

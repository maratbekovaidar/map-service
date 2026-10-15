// Command stylegen (re)builds the map style assets in deploy/tiles from pinned
// upstream sources: the styles (styles/<id>.json: the base styles and the
// themes of internal/stylegen/themes), sprites, Noto Sans glyph ranges, the
// vendored MapLibre GL JS used by the preview page and the licence texts.
//
// The output is committed to the repository, so a normal installation never
// runs this; it exists to make the assets reproducible (`make style-assets`).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"map-service/internal/config"
	"map-service/internal/stylegen"
	"map-service/internal/tiles"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stylegen:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "deploy/tiles", "output directory")
	cache := flag.String("cache", ".cache/stylegen", "download cache (gitignored)")
	glyphsZip := flag.String("glyphs-zip", "", "use this local copy of noto-sans.zip instead of downloading it (must match the pinned sha256)")
	flag.Parse()

	city, err := config.LoadCity(config.OS)
	if err != nil {
		return err
	}
	ctx := context.Background()
	hc := stylegen.DefaultHTTPClient()
	glyphStacks := []string{"Noto Sans Regular", "Noto Sans Bold", "Noto Sans Italic"}

	// 1. Base styles (+ sprites + licences), then the themes built on them.
	built := map[string][]byte{}
	for _, b := range stylegen.BaseStyles() {
		files := map[string]string{}
		for _, s := range b.Sources() {
			p, err := stylegen.Fetch(ctx, s, *cache, hc)
			if err != nil {
				return err
			}
			files[s.Name] = p
		}
		raw, err := os.ReadFile(files[b.Style.Name])
		if err != nil {
			return err
		}
		style, err := stylegen.Transform(raw, stylegen.Options{
			ID:               b.ID,
			Name:             b.Name,
			Description:      b.Description,
			DerivedFrom:      b.DerivedFrom,
			Sprite:           b.SpriteName,
			Languages:        city.Languages,
			TileMaxZoom:      tiles.MaxZoom,
			Center:           [2]float64{city.Center.Lon, city.Center.Lat},
			Zoom:             12,
			FontMap:          b.FontMap,
			ExtraAttribution: b.ExtraAttribution,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", b.ID, err)
		}
		built[b.ID] = style
		for i, s := range b.Sprites {
			if err := copyFile(files[s.Name], filepath.Join(*out, "sprites", b.SpriteName+stylegen.SpriteSuffixes[i])); err != nil {
				return err
			}
		}
		if err := copyFile(files[b.License.Name], filepath.Join(*out, "licenses", b.LicenseName)); err != nil {
			return err
		}
	}
	themes, err := stylegen.Themes()
	if err != nil {
		return err
	}
	for _, th := range themes {
		base, ok := built[th.Base]
		if !ok || built[th.ID] != nil {
			return fmt.Errorf("theme %s: base style %q is not a base style, or the id is taken", th.ID, th.Base)
		}
		style, err := stylegen.ApplyTheme(base, th, city.Languages[0])
		if err != nil {
			return err
		}
		built[th.ID] = style
	}
	for id, style := range built {
		if err := stylegen.CheckFonts(style, glyphStacks); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if err := write(filepath.Join(*out, "styles", id+".json"), style); err != nil {
			return err
		}
	}

	// 2. Glyphs.
	zipPath := *glyphsZip
	if zipPath != "" {
		if err := stylegen.VerifyFile(zipPath, stylegen.NotoGlyphs().SHA256); err != nil {
			return err
		}
	} else if zipPath, err = stylegen.Fetch(ctx, stylegen.NotoGlyphs(), *cache, hc); err != nil {
		return err
	}
	n, err := stylegen.ExtractGlyphs(zipPath, filepath.Join(*out, "fonts"), stylegen.GlyphSpec{
		Stacks: glyphStacks,
		Ranges: stylegen.DefaultRanges,
	})
	if err != nil {
		return err
	}
	lic, err := stylegen.Fetch(ctx, stylegen.NotoLicense(), *cache, hc)
	if err != nil {
		return err
	}
	if err := copyFile(lic, filepath.Join(*out, "licenses", "Noto-Sans-OFL-1.1.txt")); err != nil {
		return err
	}

	// 3. Preview page dependencies.
	for _, s := range stylegen.MapLibreFiles() {
		p, err := stylegen.Fetch(ctx, s, *cache, hc)
		if err != nil {
			return err
		}
		if err := copyFile(p, filepath.Join(*out, "vendor", s.Name)); err != nil {
			return err
		}
	}
	for _, s := range stylegen.SwaggerUIFiles() {
		p, err := stylegen.Fetch(ctx, s, *cache, hc)
		if err != nil {
			return err
		}
		if err := copyFile(p, filepath.Join(*out, "vendor", s.Name)); err != nil {
			return err
		}
	}
	for _, s := range stylegen.SwaggerUILicenses() {
		p, err := stylegen.Fetch(ctx, s, *cache, hc)
		if err != nil {
			return err
		}
		if err := copyFile(p, filepath.Join(*out, "licenses", s.Name)); err != nil {
			return err
		}
	}
	if err := write(filepath.Join(*out, "NOTICE.md"), []byte(notice)); err != nil {
		return err
	}
	fmt.Printf("style assets written to %s (%d styles, %d glyph files, languages %s)\n", *out, len(built), n, strings.Join(city.Languages, ","))
	return nil
}

func write(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return write(dst, b)
}

const notice = `# Third-party assets in deploy/tiles

Everything here is generated by ` + "`make style-assets`" + ` (cmd/stylegen) from pinned sources.

| Path | What | Source (pinned) | Licence |
|---|---|---|---|
| styles/liberty.json | OSM Liberty map style, rewired to this service (own tiles/glyphs/sprites, language-aware labels) | maputnik/osm-liberty @ da16155 | BSD (code), CC-BY 3.0 (design, derived from Mapbox OSM Bright) - licenses/OSM-Liberty-LICENSE.md |
| styles/positron.json | Positron, rewired the same way (Metropolis fonts replaced by Noto Sans) | openmaptiles/positron-gl-style @ bed9a63 | BSD (code), CC-BY 3.0 (design, CARTO basemaps by Stamen) - licenses/Positron-LICENSE.md |
| styles/dark-matter.json | Dark Matter, rewired the same way | openmaptiles/dark-matter-gl-style @ d17442c | BSD (code), CC-BY 3.0 (design, CARTO basemaps by Stamen) - licenses/Dark-Matter-LICENSE.md |
| styles/warm.json | Own theme on top of styles/liberty.json (internal/stylegen/themes/warm.json); palette modelled on a commercial map theme, only colour values reused, no third-party icons, fonts or code | this repository | as styles/liberty.json |
| sprites/osm-liberty* | OSM Liberty icon sprite sheets (Maki icons) | maputnik/osm-liberty @ da16155 | BSD / CC0 (Maki) |
| sprites/positron*, sprites/dark-matter* | Positron / Dark Matter sprite sheets | gh-pages @ 98f91af / 9769cd7 | BSD (as the styles) |
| fonts/ | Glyph ranges (PBF) of Noto Sans Regular, Bold, Italic | openmaptiles/fonts v2.0 (noto-sans.zip) | SIL Open Font License 1.1 - licenses/Noto-Sans-OFL-1.1.txt |
| vendor/ | MapLibre GL JS 5.24.0 (used only by preview.html) | npm maplibre-gl@5.24.0 | BSD-3-Clause |
| vendor/ | Swagger UI 5.32.15 (used only by the /docs page) | npm swagger-ui-dist@5.32.15 | Apache-2.0 - licenses/Swagger-UI-LICENSE.txt, bundled third-party notices in licenses/Swagger-UI-bundle-third-party.txt |
| process-openmaptiles.lua | tilemaker OpenMapTiles profile | systemed/tilemaker @ facdf93 | tilemaker FTWPL; schema CC-BY 4.0 (OpenMapTiles) |

## Attribution that must stay visible on every map

- (c) OpenStreetMap contributors - https://www.openstreetmap.org/copyright (ODbL)
- (c) OpenMapTiles - https://openmaptiles.org/ (CC-BY 4.0)

Every style carries both in the vector source's ` + "`attribution`" + ` field; MapLibre renders it in the map corner.
Positron and Dark Matter additionally require crediting CARTO (CC-BY 3.0 design); their credit is in the
style metadata (` + "`map-service:derived-from`" + `) and in the licence files above.
`

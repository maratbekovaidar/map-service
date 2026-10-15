package stylegen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Pins: every upstream input is fixed by URL and SHA-256, so regenerating the
// assets yields identical bytes or fails loudly.
const (
	// LibertyCommit is maputnik/osm-liberty (branch gh-pages), 2026-09-24.
	LibertyCommit = "da161553f7d22e91f9571e545b6af1ca42ae8e17"
	// PositronCommit and DarkMatterCommit are openmaptiles/positron-gl-style
	// (master, 2026-08-03) and openmaptiles/dark-matter-gl-style (master,
	// 2024-05-07). Their sprite sheets are only published on the gh-pages
	// branches, pinned separately (both 2024-05-06).
	PositronCommit         = "bed9a6349dfb431bd8a6646101d0977b59c591dd"
	positronSpriteCommit   = "98f91afa38671a885947d721075bdc88392c1b78"
	DarkMatterCommit       = "d17442cb66bad8c82bda59d199d5dafeead410cf"
	darkMatterSpriteCommit = "9769cd7a3642e607bb867c4c0c18bed714f52f5a"
	// fontsCommit is openmaptiles/fonts (master) at the time of pinning.
	fontsCommit = "d48c5fce2fc58b55c98d353558d807cac45e7262"

	notoZipURL     = "https://github.com/openmaptiles/fonts/releases/download/v2.0/noto-sans.zip"
	notoZipSHA     = "d117316544b43a5dde7ee761b36e17701e9f85574e181d76a74814240fdbaf34"
	notoLicenseSHA = "6a73f9541c2de74158c0e7cf6b0a58ef774f5a780bf191f2d7ec9cc53efe2bf2"

	// MapLibre GL JS 5.24.0: the last 5.x line, shipped as one UMD bundle
	// (6.x is ES-module only with a separate worker file).
	maplibreVersion = "5.24.0"

	// Swagger UI 5.32.15 (swagger-ui-dist): the bundle and stylesheet of the
	// /docs page, plus the licence texts that must ship with them.
	swaggerUIVersion = "5.32.15"
)

// Source is one pinned download.
type Source struct {
	Name   string // file name in the cache and in the output
	URL    string
	SHA256 string
}

// SpriteSuffixes are the four files of a sprite sheet, in the order of
// BaseStyle.Sprites: sprites/<name><suffix>.
var SpriteSuffixes = [4]string{".json", ".png", "@2x.json", "@2x.png"}

// BaseStyle is an upstream style that stylegen rewires to this service and
// installs as styles/<ID>.json, with its sprite sheet and licence.
type BaseStyle struct {
	ID          string
	Name        string
	Description string
	DerivedFrom string
	Style       Source
	SpriteName  string    // installed as sprites/<SpriteName><suffix>
	Sprites     [4]Source // in the order of SpriteSuffixes
	License     Source
	LicenseName string // file name under licenses/
	// ExtraAttribution is appended to the map attribution (design credit
	// the upstream licence asks to show).
	ExtraAttribution string
	// FontMap renames the upstream fonts to the glyph stacks this service
	// ships (Noto Sans Regular/Bold/Italic).
	FontMap map[string]string
}

// Sources lists every pinned download of the style.
func (b BaseStyle) Sources() []Source {
	return append([]Source{b.Style, b.License}, b.Sprites[:]...)
}

// cartoAttribution credits the design of Positron and Dark Matter (CC-BY 3.0).
const cartoAttribution = `<a href="https://carto.com/attributions" target="_blank">&copy; CARTO</a>`

// metropolis maps the fonts of the CARTO-derived OpenMapTiles styles.
var metropolis = map[string]string{
	"Metropolis Regular":       "Noto Sans Regular",
	"Metropolis Light":         "Noto Sans Regular",
	"Metropolis Medium Italic": "Noto Sans Italic",
	"Metropolis Light Italic":  "Noto Sans Italic",
	"Noto Sans Regular":        "Noto Sans Regular",
	"Noto Sans Italic":         "Noto Sans Italic",
}

// BaseStyles are the upstream styles built into deploy/tiles/styles. The
// first one is the built-in default.
func BaseStyles() []BaseStyle {
	liberty := "https://raw.githubusercontent.com/maputnik/osm-liberty/" + LibertyCommit + "/"
	positron := "https://raw.githubusercontent.com/openmaptiles/positron-gl-style/"
	dark := "https://raw.githubusercontent.com/openmaptiles/dark-matter-gl-style/"
	return []BaseStyle{
		{
			ID:          "liberty",
			Name:        "OSM Liberty",
			Description: "Colourful general-purpose map with POI icons (the classic default).",
			DerivedFrom: "OSM Liberty (maputnik/osm-liberty), BSD licence; design CC-BY 3.0 (Mapbox OSM Bright); data schema OpenMapTiles CC-BY 4.0",
			Style:       Source{"style.json", liberty + "style.json", "50012851e6d1329464308b0a4e5fcaa82e1c63da21abe3df8b968e8306e03eaf"},
			SpriteName:  "osm-liberty",
			Sprites: [4]Source{
				{"osm-liberty.json", liberty + "sprites/osm-liberty.json", "604b554c5b9f9d81fa9155d49a5a04cd64df6800bb80880e85afe844b0fd12ca"},
				{"osm-liberty.png", liberty + "sprites/osm-liberty.png", "9cbf37df0c9cf5e45663e1a3307da82c3c1c155189aafd48473912e3bff4876d"},
				{"osm-liberty@2x.json", liberty + "sprites/osm-liberty@2x.json", "9d164746c670e98c017d79c37e3ae6285addf03b48c24b42dc188fedd4787091"},
				{"osm-liberty@2x.png", liberty + "sprites/osm-liberty@2x.png", "a2227e42dc106f63932427c7997cafb4481f88228857578ea53367ae1e230661"},
			},
			License:     Source{"LICENSE-osm-liberty.md", liberty + "LICENSE.md", "932694202d771c52089e87f13105e02f2b68341f584b0f3d8adc557c9d7cefa3"},
			LicenseName: "OSM-Liberty-LICENSE.md",
			FontMap: map[string]string{
				"Roboto Regular":          "Noto Sans Regular",
				"Roboto Medium":           "Noto Sans Bold",
				"Roboto Condensed Italic": "Noto Sans Italic",
			},
		},
		{
			ID:          "positron",
			Name:        "Positron",
			Description: "Light grey, low-contrast base map for routes and markers on top.",
			DerivedFrom: "Positron (openmaptiles/positron-gl-style), BSD licence; design CC-BY 3.0 (CARTO basemaps by Stamen); data schema OpenMapTiles CC-BY 4.0",
			Style:       Source{"positron-style.json", positron + PositronCommit + "/style.json", "084275f4133cd6b1d02d852db6cb10c83a70d50571d0759617c12e6742a33099"},
			SpriteName:  "positron",
			Sprites: [4]Source{
				{"positron-sprite.json", positron + positronSpriteCommit + "/sprite.json", "25e87f3e0a663b9e234b9bc9f2f466c28de6daf30013734edf8c57ecd8ed957f"},
				{"positron-sprite.png", positron + positronSpriteCommit + "/sprite.png", "e6ed2c0d57178c469759e3a5ca4b593d82de4568c690bd01c7f8b57e8f38ccab"},
				{"positron-sprite@2x.json", positron + positronSpriteCommit + "/sprite@2x.json", "0f11263302107aef560f50e69247b1d54b6abe5fa6e7ee210e1d8dd7078f12a5"},
				{"positron-sprite@2x.png", positron + positronSpriteCommit + "/sprite@2x.png", "cdf987ec722a33376f8e044ff5103662cfd1e7ea5151f0600b274117d8c01709"},
			},
			License:          Source{"LICENSE-positron.md", positron + PositronCommit + "/LICENSE.md", "7bc994370067d4160209ea9bc85239403871284c2c930c34ddc792723d05f89f"},
			LicenseName:      "Positron-LICENSE.md",
			ExtraAttribution: cartoAttribution,
			FontMap:          metropolis,
		},
		{
			ID:          "dark-matter",
			Name:        "Dark Matter",
			Description: "Dark base map for night mode.",
			DerivedFrom: "Dark Matter (openmaptiles/dark-matter-gl-style), BSD licence; design CC-BY 3.0 (CARTO basemaps by Stamen); data schema OpenMapTiles CC-BY 4.0",
			Style:       Source{"dark-matter-style.json", dark + DarkMatterCommit + "/style.json", "0e3e1f4b26a1af8243f7fbbb89f3afec530c2eafbf5ba41c90210c40508c843d"},
			SpriteName:  "dark-matter",
			Sprites: [4]Source{
				{"dark-matter-sprite.json", dark + darkMatterSpriteCommit + "/sprite.json", "cd1381fba4b34c1e5654b7b95d9ba6b704a9c526a5fec924b7ec2da1c4404376"},
				{"dark-matter-sprite.png", dark + darkMatterSpriteCommit + "/sprite.png", "3605b162b28b0b8354e95b4e20586f313ad90b5f37fd2686fa53bb70d038d3f6"},
				{"dark-matter-sprite@2x.json", dark + darkMatterSpriteCommit + "/sprite@2x.json", "9b391ebf72fa60884123548c6761f6efacc8d96a4247443293360573c0eb1710"},
				{"dark-matter-sprite@2x.png", dark + darkMatterSpriteCommit + "/sprite@2x.png", "54a6c9dd09ff97ee844c6109823b43f4aa37d1fcd3595dc97ec8076c82247549"},
			},
			License:          Source{"LICENSE-dark-matter.md", dark + DarkMatterCommit + "/LICENSE.md", "7bc994370067d4160209ea9bc85239403871284c2c930c34ddc792723d05f89f"},
			LicenseName:      "Dark-Matter-LICENSE.md",
			ExtraAttribution: cartoAttribution,
			FontMap:          metropolis,
		},
	}
}

// NotoGlyphs is the pinned glyph archive (62 MB; only a few ranges are used).
func NotoGlyphs() Source { return Source{"noto-sans.zip", notoZipURL, notoZipSHA} }

// NotoLicense is the SIL OFL text of the fonts.
func NotoLicense() Source {
	return Source{"LICENSE-noto-sans-OFL.txt",
		"https://raw.githubusercontent.com/openmaptiles/fonts/" + fontsCommit + "/noto-sans/LICENSE", notoLicenseSHA}
}

// MapLibreFiles are the vendored browser library files of the preview page.
func MapLibreFiles() []Source {
	base := "https://cdn.jsdelivr.net/npm/maplibre-gl@" + maplibreVersion + "/dist/"
	return []Source{
		{"maplibre-gl.js", base + "maplibre-gl.js", "45a9b07a9189ce56054c620a947ccf41e291e58c95e9b61533b740aaa65ee5cb"},
		{"maplibre-gl.css", base + "maplibre-gl.css", "ab1e70d59ec40465bae7e7030da2f3ccf28133fd502e62bd598eefbadfd7a732"},
	}
}

// SwaggerUIFiles are the vendored browser files of the /docs page.
func SwaggerUIFiles() []Source {
	base := "https://cdn.jsdelivr.net/npm/swagger-ui-dist@" + swaggerUIVersion + "/"
	return []Source{
		{"swagger-ui-bundle.js", base + "swagger-ui-bundle.js", "a7e344f2770b2f07527ce828e0951626983b8f2dcdb7a826689c0232023f995b"},
		{"swagger-ui.css", base + "swagger-ui.css", "d7f39f764aa18c7b47dd05b9af5613e373e4ac0f3557c2693d52d0abc2464d76"},
	}
}

// SwaggerUILicenses are the Apache-2.0 text of Swagger UI and the notices of
// the third-party code inside its bundle.
func SwaggerUILicenses() []Source {
	base := "https://cdn.jsdelivr.net/npm/swagger-ui-dist@" + swaggerUIVersion + "/"
	return []Source{
		{"Swagger-UI-LICENSE.txt", base + "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"},
		{"Swagger-UI-bundle-third-party.txt", base + "swagger-ui-bundle.js.LICENSE.txt", "63818894e4b04cd0e3180d9cb20761e227a939121e7484f8e1d528227c756f89"},
	}
}

// VerifyFile checks that path has the given SHA-256.
func VerifyFile(path, wantSHA string) error {
	ok, err := fileHasHash(path, wantSHA)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("stylegen: %s does not match the pinned sha256 %s", path, wantSHA)
	}
	return nil
}

// Fetch downloads s into cacheDir (unless a verified copy is already there)
// and returns its path. A hash mismatch is always an error.
func Fetch(ctx context.Context, s Source, cacheDir string, hc *http.Client) (string, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(cacheDir, s.Name)
	if ok, _ := fileHasHash(dst, s.SHA256); ok {
		return dst, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("stylegen: download %s: %w", s.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stylegen: download %s: HTTP %s", s.URL, resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("stylegen: download %s: %w", s.URL, err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != s.SHA256 {
		os.Remove(tmp)
		return "", fmt.Errorf("stylegen: %s: sha256 mismatch: got %s, pinned %s", s.Name, got, s.SHA256)
	}
	return dst, os.Rename(tmp, dst)
}

func fileHasHash(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// DefaultHTTPClient returns a client suited to the slow GitHub release CDN.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 45 * time.Minute}
}

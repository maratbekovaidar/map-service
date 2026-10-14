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

// LibertyFiles are the OSM Liberty files copied/derived from the pinned commit.
func LibertyFiles() []Source {
	base := "https://raw.githubusercontent.com/maputnik/osm-liberty/" + LibertyCommit + "/"
	return []Source{
		{"style.json", base + "style.json", "50012851e6d1329464308b0a4e5fcaa82e1c63da21abe3df8b968e8306e03eaf"},
		{"osm-liberty.json", base + "sprites/osm-liberty.json", "604b554c5b9f9d81fa9155d49a5a04cd64df6800bb80880e85afe844b0fd12ca"},
		{"osm-liberty.png", base + "sprites/osm-liberty.png", "9cbf37df0c9cf5e45663e1a3307da82c3c1c155189aafd48473912e3bff4876d"},
		{"osm-liberty@2x.json", base + "sprites/osm-liberty@2x.json", "9d164746c670e98c017d79c37e3ae6285addf03b48c24b42dc188fedd4787091"},
		{"osm-liberty@2x.png", base + "sprites/osm-liberty@2x.png", "a2227e42dc106f63932427c7997cafb4481f88228857578ea53367ae1e230661"},
		{"LICENSE-osm-liberty.md", base + "LICENSE.md", "932694202d771c52089e87f13105e02f2b68341f584b0f3d8adc557c9d7cefa3"},
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

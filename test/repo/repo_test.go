// Package repo holds repository-level guards: the example configuration is
// valid and complete, and every container image is pinned.
package repo

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"map-service/internal/config"
	"map-service/internal/pipeline"
	"map-service/internal/stylegen"
	"map-service/internal/styles"
)

func root(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

// exampleEnv parses .env.example (KEY=VALUE lines).
func exampleEnv(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(root(t), ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if strings.ContainsAny(v, `"'`) {
			t.Errorf(".env.example: %s must not be quoted (compose, make and the Go services parse the file differently): %s", k, v)
		}
		env[strings.TrimSpace(k)] = v
	}
	return env
}

// variables set by compose/Docker or meant for tests, not for the operator.
var internalOnly = map[string]bool{
	"DATA_ROOT": true, "STATIC_DIR": true, "STATE_DIR": true, // fixed inside the containers
	"GATEWAY_LISTEN_ADDR": true, "TRIPS_LISTEN_ADDR": true,
	"REPO_DIR": true, "COMPOSE_FILE_PATH": true, "COMPOSE_ENV_FILE": true, // set by the Makefile/compose for the updater
	"OSMIUM_BIN": true, "DOCKER_BIN": true, "LOG_FORMAT": true, // tool locations / log style of the updater
}

// spy returns a Getenv that serves values from env and records every key asked for.
func spy(env map[string]string, seen map[string]bool) config.Getenv {
	return func(k string) string { seen[k] = true; return env[k] }
}

func undocumented(seen map[string]bool, env map[string]string) []string {
	var missing []string
	for k := range seen {
		if _, ok := env[k]; !ok && !internalOnly[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

func TestExampleConfigIsValidAndDocumentsEveryVariable(t *testing.T) {
	env := exampleEnv(t)
	seen := map[string]bool{}

	if _, err := config.LoadGateway(spy(env, seen)); err != nil {
		t.Errorf(".env.example is not a valid gateway configuration: %v", err)
	}
	if _, err := config.LoadTrips(spy(env, seen)); err != nil {
		t.Errorf(".env.example is not a valid trips configuration: %v", err)
	}
	env["REPO_DIR"] = "/srv/map-service" // the updater requires an absolute path
	if _, err := pipeline.LoadConfig(spy(env, seen)); err != nil {
		t.Errorf(".env.example is not a valid updater configuration: %v", err)
	}
	if missing := undocumented(seen, env); len(missing) > 0 {
		t.Errorf("variables read by the code but missing from .env.example: %v", missing)
	}
}

var composeVars = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`)

// Every ${VAR} used by the compose files must be defined in .env.example or be
// provided by the Makefile.
func TestComposeVariablesAreDocumented(t *testing.T) {
	env := exampleEnv(t)
	fromMakefile := map[string]bool{"DATA_DIR": true, "DATA_VERSION": true, "REPO_DIR": true, "PIPELINE_VERSION": true, "OSRM_PROFILE": true}
	for _, name := range []string{"docker-compose.yml", "docker-compose.dev.yml"} {
		b, err := os.ReadFile(filepath.Join(root(t), "deploy", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range composeVars.FindAllStringSubmatch(string(b), -1) {
			if _, ok := env[m[1]]; !ok && !fromMakefile[m[1]] {
				t.Errorf("%s uses ${%s}, which is neither in .env.example nor exported by the Makefile/updater", name, m[1])
			}
		}
	}
}

// All images must carry an explicit tag or digest, never "latest".
func TestComposeImagesArePinned(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "deploy", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
			Build any    `yaml:"build"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Services) < 8 {
		t.Fatalf("parsed only %d services", len(doc.Services))
	}
	for name, svc := range doc.Services {
		img := svc.Image
		if img == "" {
			t.Errorf("service %s has no image", name)
			continue
		}
		if strings.Contains(img, "${") {
			// locally built images (project-prefixed) must still carry an explicit tag
			if !strings.Contains(img, ":") || strings.HasSuffix(img, ":latest") {
				t.Errorf("service %s: image %q needs an explicit non-latest tag", name, img)
			}
			continue
		}
		if !pinned(img) {
			t.Errorf("service %s: image %q is not pinned to a tag or digest", name, img)
		}
	}
}

func pinned(ref string) bool {
	if strings.Contains(ref, "@sha256:") {
		return true
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") { // a ":" before the registry path is a port
		return false
	}
	return ref[i+1:] != "latest" && ref[i+1:] != ""
}

func TestDockerfileBasesArePinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(root(t), "deploy", "*Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob(filepath.Join(root(t), "deploy", "*", "Dockerfile"))
	files = append(files, more...)
	if len(files) < 3 {
		t.Fatalf("found only %d Dockerfiles", len(files))
	}
	from := regexp.MustCompile(`(?im)^FROM\s+(?:--platform=\S+\s+)?(\S+)`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range from.FindAllStringSubmatch(string(b), -1) {
			if m[1] == "scratch" {
				continue
			}
			if !pinned(m[1]) {
				t.Errorf("%s: base image %q is not pinned", filepath.Base(f), m[1])
			}
		}
	}
}

func TestPinnedDetector(t *testing.T) {
	for ref, want := range map[string]bool{
		"redis":                  false,
		"redis:latest":           false,
		"redis:":                 false,
		"localhost:5000/redis":   false, // a port is not a tag
		"redis:8.10.2-alpine":    true,
		"localhost:5000/redis:8": true,
		"ghcr.io/systemed/tilemaker@sha256:d3eda2790458de727bb0096eebce775984fc060f60901dac6dbfbbb79ae17370": true,
		"mediagis/nominatim:5.3.2": true,
	} {
		if got := pinned(ref); got != want {
			t.Errorf("pinned(%q) = %v, want %v", ref, got, want)
		}
	}
}

// Every committed style names itself correctly and references only sprites
// and fonts the gateway ships, and the themed styles are up to date with
// internal/stylegen/themes (editing a theme without `make style-assets`
// fails here).
func TestCommittedStyles(t *testing.T) {
	tilesDir := filepath.Join(root(t), "deploy", "tiles")
	entries, err := os.ReadDir(filepath.Join(tilesDir, "styles"))
	if err != nil {
		t.Fatal(err)
	}
	fontDirs, err := os.ReadDir(filepath.Join(tilesDir, "fonts"))
	if err != nil {
		t.Fatal(err)
	}
	var stacks []string
	for _, e := range fontDirs {
		stacks = append(stacks, e.Name())
	}
	built := map[string][]byte{}
	langs := map[string]string{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !styles.ValidID(id) {
			t.Errorf("styles/%s: not a valid style file name", e.Name())
			continue
		}
		b, err := os.ReadFile(filepath.Join(tilesDir, "styles", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Name     string         `json:"name"`
			Sprite   string         `json:"sprite"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Errorf("styles/%s: %v", e.Name(), err)
			continue
		}
		if doc.Name == "" || doc.Metadata[stylegen.MetaStyleID] != id || doc.Metadata[stylegen.MetaDescription] == "" {
			t.Errorf("styles/%s: name/metadata: %q %v", e.Name(), doc.Name, doc.Metadata)
		}
		if l, _ := doc.Metadata[stylegen.MetaLanguages].([]any); len(l) > 0 {
			langs[id], _ = l[0].(string)
		}
		if doc.Sprite != "" {
			name, ok := strings.CutPrefix(doc.Sprite, stylegen.PlaceholderBaseURL+"/tiles/sprites/")
			if !ok {
				t.Errorf("styles/%s: sprite %q is not served by the gateway", e.Name(), doc.Sprite)
			}
			for _, suffix := range stylegen.SpriteSuffixes {
				if _, err := os.Stat(filepath.Join(tilesDir, "sprites", name+suffix)); err != nil {
					t.Errorf("styles/%s: %v", e.Name(), err)
				}
			}
		}
		if err := stylegen.CheckFonts(b, stacks); err != nil {
			t.Errorf("styles/%s: %v", e.Name(), err)
		}
		built[id] = b
	}
	if _, ok := built["liberty"]; !ok {
		t.Error("styles/liberty.json (the built-in DEFAULT_STYLE) is missing")
	}
	themes, err := stylegen.Themes()
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range themes {
		want, err := stylegen.ApplyTheme(built[th.Base], th, langs[th.Base])
		if err != nil {
			t.Errorf("theme %s: %v", th.ID, err)
			continue
		}
		if string(want) != string(built[th.ID]) {
			t.Errorf("deploy/tiles/styles/%s.json is stale: run `make style-assets` after editing internal/stylegen/themes/%s.json", th.ID, th.ID)
		}
	}
}

// Caddy logs the whole request - URI and all headers - on every access and
// error line, and redacts only Cookie/Authorization by default. The API key
// travels in X-API-Key and, for the preview page, in ?key=, so the Caddyfile
// must redact both in every log block and must do it without mangling the rest
// of the URI (the `query` filter drops pairs containing ";", i.e. the route
// "points" parameter). This checks the committed rules, not Caddy itself.
func TestCaddyLogsRedactAPIKeys(t *testing.T) {
	read := func(parts ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{root(t)}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	caddyfile := read("deploy", "Caddyfile")

	// Access lines go through the site `log`, error lines through the global
	// one: both must use the filter snippet, and none may fall back to plain json.
	logBlocks := len(regexp.MustCompile(`(?m)^\s*log\s*\{`).FindAllString(caddyfile, -1))
	imports := len(regexp.MustCompile(`(?m)^\s*import\s+redact_api_keys\s*$`).FindAllString(caddyfile, -1))
	if logBlocks != 2 || imports != logBlocks {
		t.Errorf("Caddyfile has %d log blocks and %d `import redact_api_keys`: the global and the site log must each apply the redaction", logBlocks, imports)
	}
	if regexp.MustCompile(`(?m)^\s*format\s+json\b`).MatchString(caddyfile) {
		t.Error("Caddyfile: a log block uses plain `format json`, which logs API keys; use `format filter` with `import redact_api_keys`")
	}

	if !regexp.MustCompile(`(?m)^\s*request>headers>X-Api-Key\s+(delete|hash|replace\s+\S+)\s*$`).MatchString(caddyfile) {
		t.Error("Caddyfile: the X-Api-Key request header (canonical form of X-API-Key) is not redacted in logs")
	}

	// The same pattern must cover the URI and the Referer (which echoes the preview URL).
	rule := regexp.MustCompile(`(?m)^\s*request>(uri|headers>Referer)\s+regexp\s+"([^"]+)"\s+"([^"]*)"\s*$`)
	var pattern, repl string
	seen := map[string]bool{}
	for _, m := range rule.FindAllStringSubmatch(caddyfile, -1) {
		seen[m[1]] = true
		if pattern != "" && (pattern != m[2] || repl != m[3]) {
			t.Errorf("Caddyfile: the uri and Referer redaction rules differ")
		}
		pattern, repl = m[2], m[3]
	}
	if !seen["uri"] || !seen["headers>Referer"] {
		t.Fatalf("Caddyfile: want a regexp redaction rule for both request>uri and request>headers>Referer, found %v", seen)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("Caddyfile redaction pattern %q: %v", pattern, err)
	}
	for in, want := range map[string]string{
		"/tiles/preview.html?key=SECRET":              "/tiles/preview.html?key=REDACTED",
		"/v1/geocode?q=a&key=SECRET&x=1":              "/v1/geocode?q=a&key=REDACTED&x=1",
		"/v1/route/car?points=1,2;3,4&key=SECRET":     "/v1/route/car?points=1,2;3,4&key=REDACTED",
		"/v1/route/car?points=1,2;3,4&steps=true":     "/v1/route/car?points=1,2;3,4&steps=true", // ";" pairs survive
		"/v1/x?a=1;key=SECRET;b=2":                    "/v1/x?a=1;key=REDACTED;b=2",
		"http://h/tiles/preview.html?key=SECRET#frag": "http://h/tiles/preview.html?key=REDACTED#frag",
		"/v1/x?monkey=1&keys=2":                       "/v1/x?monkey=1&keys=2", // only the parameter named exactly "key"
		"/v1/reverse?lat=51.1&lon=71.4":               "/v1/reverse?lat=51.1&lon=71.4",
	} {
		if got := re.ReplaceAllString(in, repl); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}

	// The preview page is what puts the key into the URL; if it ever reads
	// another parameter, the pattern above has to follow.
	if !strings.Contains(read("deploy", "tiles", "preview.html"), "params.get('key')") {
		t.Error("deploy/tiles/preview.html no longer reads the key from ?key=: update the Caddyfile redaction (and this test) to the new parameter")
	}
}

// The preview page plays a trip's driver and watcher. Trip tokens must never
// reach a URL the server sees (and logs): the watcher's link carries the token
// in the #fragment, the page takes it out of the address bar, and tokens only
// travel in the Authorization header.
func TestPreviewKeepsTripTokensOutOfURLs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "deploy", "tiles", "preview.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{
		"new URLSearchParams(location.hash.slice(1))",                         // the link is read from the fragment...
		"history.replaceState(null, '', location.pathname + location.search)", // ...and removed from the address bar
		"'#trip=' + t.trip.id + '&watch=' + t.watch_token",                    // links are built with a fragment
		"Authorization: 'Bearer ' + token",                                    // tokens go in a header
	} {
		if !strings.Contains(page, want) {
			t.Errorf("preview.html no longer contains %q", want)
		}
	}
	for _, bad := range []string{"params.get('watch')", "params.get('token')", "searchParams.set('watch'", "?watch=", "?trip="} {
		if strings.Contains(page, bad) {
			t.Errorf("preview.html puts a trip token into a query string (%q)", bad)
		}
	}
}

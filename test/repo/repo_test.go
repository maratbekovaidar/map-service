// Package repo holds repository-level guards: the example configuration is
// valid and complete, and every container image is pinned.
package repo

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"map-service/internal/config"
	"map-service/internal/pipeline"
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
	"DATA_ROOT": true, "STATIC_DIR": true, "GATEWAY_LISTEN_ADDR": true, // fixed inside the gateway container
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

package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"map-service/internal/config"
	"map-service/internal/dataset"
	"map-service/internal/geo"
)

// fakeRunner records commands and lets tests script their results.
type fakeRunner struct {
	mu    sync.Mutex
	runs  []Cmd
	outs  []Cmd
	onRun func(Cmd) error
	onOut func(Cmd) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, _ *slog.Logger, c Cmd) error {
	f.mu.Lock()
	f.runs = append(f.runs, c)
	hook := f.onRun
	f.mu.Unlock()
	if hook != nil {
		return hook(c)
	}
	return nil
}

func (f *fakeRunner) Output(_ context.Context, c Cmd) (string, error) {
	f.mu.Lock()
	f.outs = append(f.outs, c)
	hook := f.onOut
	f.mu.Unlock()
	if hook != nil {
		return hook(c)
	}
	return "", nil
}

func (f *fakeRunner) joined() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.runs))
	for i, c := range f.runs {
		out[i] = c.Name + " " + strings.Join(c.Args, " ")
	}
	return out
}

func (f *fakeRunner) ran(substr string) bool {
	for _, l := range f.joined() {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	repo := t.TempDir()
	for _, d := range []string{"deploy/tiles", "deploy/osrm"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tmpl := `{"layers":{"place":{"minzoom":0,"maxzoom":14}},"settings":{"minzoom":0,"maxzoom":14,"compress":"gzip"}}`
	if err := os.WriteFile(filepath.Join(repo, "deploy/tiles/config-openmaptiles.json"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	bbox, _ := geo.ParseBBox("71.04,50.74,71.96,51.46")
	return &Config{
		City:                   config.City{Name: "Astana", BBox: bbox, Profiles: []string{"car", "foot"}, Languages: []string{"ru", "kk", "en"}},
		SourceURL:              "https://example.test/kz.osm.pbf",
		ExtractMode:            ExtractBBox,
		ExtractStrategy:        "smart",
		DataDir:                filepath.Join(repo, "data"),
		RepoDir:                repo,
		ComposeFile:            filepath.Join(repo, "deploy/docker-compose.yml"),
		EnvFile:                filepath.Join(repo, ".env"),
		Project:                "mapsvc",
		KeepVersions:           2,
		NominatimPassword:      "s3cret",
		NominatimImportTimeout: time.Second,
		OsmiumBin:              "osmium",
		DockerBin:              "docker",
	}
}

func newTestBuild(t *testing.T, cfg *Config, fr *fakeRunner) *Build {
	t.Helper()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := NewBuild(cfg, "20261014T060000Z", slog.New(slog.NewTextHandler(io.Discard, nil)), fr, http.DefaultClient)
	b.PollInterval = time.Millisecond
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLoadConfig(t *testing.T) {
	get := func(m map[string]string) config.Getenv { return func(k string) string { return m[k] } }
	base := map[string]string{
		"OSM_SOURCE_URL":     "https://example.test/x.pbf",
		"NOMINATIM_PASSWORD": "pw",
		"REPO_DIR":           "/srv/map",
	}
	c, err := LoadConfig(get(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/srv/map/data" || c.ComposeFile != "/srv/map/deploy/docker-compose.yml" ||
		c.KeepVersions != 2 || c.Cron != "0 3 * * 0" || c.ExtractMode != "bbox" || c.ExtractStrategy != "smart" {
		t.Errorf("defaults wrong: %+v", c)
	}

	bad := map[string]string{
		"OSM_SOURCE_URL":     "",
		"EXTRACT_MODE":       "polygon",
		"EXTRACT_STRATEGY":   "weird",
		"DATA_KEEP_VERSIONS": "0",
		"DATA_DIR":           "relative/dir",
		"REPO_DIR":           "/srv/map",
	}
	_, err = LoadConfig(get(bad))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"OSM_SOURCE_URL is required", "NOMINATIM_PASSWORD is required", "requires CITY_POLYGON_FILE", "EXTRACT_STRATEGY", "DATA_KEEP_VERSIONS", "must be an absolute path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestParseSource(t *testing.T) {
	cases := []struct {
		in   string
		kind sourceKind
		loc  string
		ok   bool
	}{
		{"https://download.geofabrik.de/asia/kazakhstan-latest.osm.pbf", sourceHTTP, "https://download.geofabrik.de/asia/kazakhstan-latest.osm.pbf", true},
		{"file:///data/sources/kz.osm.pbf", sourceFile, "/data/sources/kz.osm.pbf", true},
		{"/data/sources/kz.osm.pbf", sourceFile, "/data/sources/kz.osm.pbf", true},
		{"relative/kz.osm.pbf", 0, "", false},
		{"ftp://host/kz.osm.pbf", 0, "", false},
		{"https:///nohost.pbf", 0, "", false},
	}
	for _, c := range cases {
		kind, loc, err := ParseSource(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseSource(%q) err = %v, ok want %v", c.in, err, c.ok)
			continue
		}
		if c.ok && (kind != c.kind || loc != c.loc) {
			t.Errorf("ParseSource(%q) = %v %q, want %v %q", c.in, kind, loc, c.kind, c.loc)
		}
	}
}

func TestDownloadHTTPWithETagRevalidation(t *testing.T) {
	body := bytes.Repeat([]byte("osm"), 1000)
	var hits, conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.SourceURL = srv.URL + "/kz.osm.pbf"
	fr := &fakeRunner{onOut: func(c Cmd) (string, error) { return "2026-10-13T20:21:31Z\n", nil }}
	b := newTestBuild(t, cfg, fr)

	if err := b.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(cfg.DataDir, "cache", "source.osm.pbf")
	got, _ := os.ReadFile(cached)
	if !bytes.Equal(got, body) {
		t.Fatalf("cached file has %d bytes, want %d", len(got), len(body))
	}
	if b.source.OSMTimestamp != "2026-10-13T20:21:31Z" || b.source.SizeBytes != int64(len(body)) {
		t.Errorf("source info: %+v", b.source)
	}

	// Second run revalidates and reuses the cache.
	if err := b.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 || conditional != 1 {
		t.Errorf("hits=%d conditional=%d, want 2 and 1", hits, conditional)
	}
	if _, err := os.Stat(cached + ".part"); err == nil {
		t.Error("temporary .part file must not remain")
	}
}

func TestDownloadRejectsTruncatedAndHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("only a few bytes"))
	}))
	defer srv.Close()
	cfg := testConfig(t)
	b := newTestBuild(t, cfg, &fakeRunner{})

	cfg.SourceURL = srv.URL + "/short"
	if err := b.Download(context.Background()); err == nil {
		t.Error("truncated download must fail")
	}
	cached := filepath.Join(cfg.DataDir, "cache", "source.osm.pbf")
	if _, err := os.Stat(cached); err == nil {
		t.Error("a truncated download must not be published as the cache file")
	}
	if _, err := os.Stat(cached + ".part"); err == nil {
		t.Error(".part file must be removed after a failed download")
	}

	cfg.SourceURL = srv.URL + "/missing"
	err := b.Download(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("want 404 error, got %v", err)
	}
}

func TestDownloadLocalFile(t *testing.T) {
	cfg := testConfig(t)
	src := filepath.Join(t.TempDir(), "local.osm.pbf")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.SourceURL = "file://" + src
	b := newTestBuild(t, cfg, &fakeRunner{onOut: func(Cmd) (string, error) { return "", errors.New("no header") }})
	if err := b.Download(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.sourcePath != src {
		t.Errorf("local file must be used in place, got %q", b.sourcePath)
	}
	cfg.SourceURL = "file://" + src + ".missing"
	if err := b.Download(context.Background()); err == nil {
		t.Error("missing local file must fail")
	}
}

const fileInfoJSON = `{"data":{"count":{"nodes":%d,"ways":%d,"relations":10}}}`

func extractRunner(nodes, ways int) *fakeRunner {
	return &fakeRunner{onOut: func(c Cmd) (string, error) {
		if len(c.Args) > 0 && c.Args[0] == "fileinfo" {
			return fmt.Sprintf(fileInfoJSON, nodes, ways), nil
		}
		return "", nil
	}}
}

func TestExtractBBoxCommand(t *testing.T) {
	cfg := testConfig(t)
	fr := extractRunner(5000, 800)
	b := newTestBuild(t, cfg, fr)
	b.sourcePath = "/src/kz.osm.pbf"
	if err := b.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "osmium extract --strategy smart -S types=multipolygon,boundary --bbox 71.04,50.74,71.96,51.46 --set-bounds --overwrite -o " +
		filepath.Join(b.Dir, "city.osm.pbf") + " /src/kz.osm.pbf"
	if got := fr.joined(); len(got) != 1 || got[0] != want {
		t.Errorf("command:\n got  %v\n want %s", got, want)
	}
}

func TestExtractPolygonMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.ExtractMode = ExtractPolygon
	cfg.ExtractStrategy = "complete_ways"
	cfg.PolygonFile = filepath.Join(t.TempDir(), "astana.geojson")
	fr := extractRunner(5000, 800)
	b := newTestBuild(t, cfg, fr)
	b.sourcePath = "/src/kz.osm.pbf"

	if err := b.Extract(context.Background()); err == nil {
		t.Fatal("missing polygon file must fail")
	}
	if err := os.WriteFile(cfg.PolygonFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}
	line := fr.joined()[0]
	if !strings.Contains(line, "--polygon "+cfg.PolygonFile) || strings.Contains(line, "--bbox") || strings.Contains(line, "-S types") {
		t.Errorf("polygon command wrong: %s", line)
	}
}

func TestExtractRejectsEmptyResult(t *testing.T) {
	cfg := testConfig(t)
	b := newTestBuild(t, cfg, extractRunner(12, 1))
	b.sourcePath = "/src/kz.osm.pbf"
	err := b.Extract(context.Background())
	if err == nil || !strings.Contains(err.Error(), "looks empty") {
		t.Fatalf("want 'looks empty' error, got %v", err)
	}
}

func TestOSRMBuildsEveryProfile(t *testing.T) {
	cfg := testConfig(t)
	// A custom profile for foot must be preferred over the stock one.
	if err := os.WriteFile(filepath.Join(cfg.RepoDir, "deploy/osrm/foot.lua"), []byte("-- custom"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{}
	b := newTestBuild(t, cfg, fr)
	if err := os.WriteFile(b.path("city.osm.pbf"), []byte("pbf"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr.onRun = func(c Cmd) error { // emulate osrm-extract writing output
		if strings.Contains(strings.Join(c.Args, " "), "osrm-extract") {
			dir := b.path("osrm", c.Env["OSRM_PROFILE"])
			return os.WriteFile(filepath.Join(dir, "city.osrm"), []byte("x"), 0o644)
		}
		return nil
	}
	if err := b.OSRM(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fr.joined()
	if len(got) != 6 {
		t.Fatalf("want 3 commands x 2 profiles, got %d:\n%s", len(got), strings.Join(got, "\n"))
	}
	for i, want := range []string{
		"run --rm --no-deps -T osrm-tools osrm-extract -p /opt/car.lua /data/city.osm.pbf",
		"osrm-tools osrm-partition /data/city.osrm",
		"osrm-tools osrm-customize /data/city.osrm",
		"osrm-tools osrm-extract -p /profiles/foot.lua /data/city.osm.pbf",
	} {
		if !strings.Contains(got[i], want) {
			t.Errorf("command %d = %q, want it to contain %q", i, got[i], want)
		}
	}
	if fr.runs[0].Env["OSRM_PROFILE"] != "car" || fr.runs[3].Env["OSRM_PROFILE"] != "foot" {
		t.Errorf("profile env wrong: %v / %v", fr.runs[0].Env, fr.runs[3].Env)
	}
	if fr.runs[0].Env["PIPELINE_VERSION"] != b.Version || fr.runs[0].Env["DATA_VERSION"] != "none" {
		t.Errorf("version env wrong: %v", fr.runs[0].Env)
	}
	// The per-profile hard link of the PBF must not be left behind.
	for _, p := range []string{"car", "foot"} {
		if _, err := os.Stat(b.path("osrm", p, "city.osm.pbf")); err == nil {
			t.Errorf("%s: temporary PBF copy not removed", p)
		}
	}
}

func TestOSRMStopsOnFailureAndNamesProfile(t *testing.T) {
	cfg := testConfig(t)
	fr := &fakeRunner{onRun: func(c Cmd) error {
		if strings.Contains(strings.Join(c.Args, " "), "osrm-partition") {
			return errors.New("exit status 1")
		}
		return nil
	}}
	b := newTestBuild(t, cfg, fr)
	_ = os.WriteFile(b.path("city.osm.pbf"), []byte("pbf"), 0o644)
	err := b.OSRM(context.Background())
	if err == nil || !strings.Contains(err.Error(), "osrm car") || !strings.Contains(err.Error(), "osrm-partition") {
		t.Fatalf("error should name the profile and tool, got %v", err)
	}
	if fr.ran("osrm-extract -p /opt/foot.lua") {
		t.Error("must not continue with the next profile after a failure")
	}
}

func TestTilesWritesCityConfigAndRunsTilemaker(t *testing.T) {
	cfg := testConfig(t)
	fr := &fakeRunner{}
	b := newTestBuild(t, cfg, fr)
	fr.onRun = func(c Cmd) error { // emulate tilemaker producing a PMTiles archive
		hdr := append([]byte("PMTiles\x03"), make([]byte, 2000)...)
		return os.WriteFile(b.path("tiles", "city.pmtiles"), hdr, 0o644)
	}
	if err := b.Tiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(b.path("tilemaker.json"))
	if err != nil {
		t.Fatal(err)
	}
	var generated struct {
		Settings struct {
			BoundingBox []float64 `json:"bounding_box"`
			Name        string    `json:"name"`
			Compress    string    `json:"compress"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &generated); err != nil {
		t.Fatal(err)
	}
	bb := generated.Settings.BoundingBox
	if len(bb) != 4 || bb[0] != 71.04 || bb[3] != 51.46 || generated.Settings.Compress != "gzip" || !strings.Contains(generated.Settings.Name, "Astana") {
		t.Errorf("generated tilemaker config: %+v", generated.Settings)
	}
	line := fr.joined()[0]
	for _, want := range []string{"run --rm --no-deps -T tilemaker", "--input /data/city.osm.pbf", "--output /data/tiles/city.pmtiles", "--config /data/tilemaker.json", "--process /config/process-openmaptiles.lua"} {
		if !strings.Contains(line, want) {
			t.Errorf("tilemaker command lacks %q: %s", want, line)
		}
	}
}

func TestCheckPMTiles(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.pmtiles")
	_ = os.WriteFile(good, append([]byte("PMTiles\x03"), make([]byte, 2000)...), 0o644)
	if err := CheckPMTiles(good); err != nil {
		t.Errorf("valid archive rejected: %v", err)
	}
	for name, content := range map[string][]byte{
		"v2.pmtiles":    append([]byte("PMTiles\x02"), make([]byte, 2000)...),
		"junk.pmtiles":  append([]byte("NOTATILES"), make([]byte, 2000)...),
		"small.pmtiles": []byte("PMTiles\x03"),
	} {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, content, 0o644)
		if err := CheckPMTiles(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := CheckPMTiles(filepath.Join(dir, "missing.pmtiles")); err == nil {
		t.Error("missing archive accepted")
	}
}

func TestWaitHealthy(t *testing.T) {
	cfg := testConfig(t)
	states := []string{"running starting", "running starting", "running healthy"}
	var n int
	fr := &fakeRunner{onOut: func(c Cmd) (string, error) {
		s := states[min(n, len(states)-1)]
		n++
		return s + "\n", nil
	}}
	b := newTestBuild(t, cfg, fr)
	if err := b.waitHealthy(context.Background(), "c", time.Second); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
	if n != 3 {
		t.Errorf("polled %d times, want 3", n)
	}

	// A container that exited is reported with its last log lines.
	fr2 := &fakeRunner{onOut: func(c Cmd) (string, error) {
		if c.Args[0] == "logs" {
			return "ERROR: import failed\n", nil
		}
		return "exited \n", nil
	}}
	b2 := newTestBuild(t, cfg, fr2)
	err := b2.waitHealthy(context.Background(), "c", time.Second)
	if err == nil || !strings.Contains(err.Error(), "import failed") {
		t.Errorf("exited container: %v", err)
	}

	// Never healthy -> timeout.
	fr3 := &fakeRunner{onOut: func(Cmd) (string, error) { return "running starting\n", nil }}
	b3 := newTestBuild(t, cfg, fr3)
	if err := b3.waitHealthy(context.Background(), "c", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("want timeout, got %v", err)
	}

	// Cancellation is honoured.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b3.waitHealthy(ctx, "c", time.Hour); err == nil {
		t.Error("cancelled context must abort the wait")
	}
}

func TestPhotonImportCommand(t *testing.T) {
	cfg := testConfig(t)
	fr := &fakeRunner{}
	b := newTestBuild(t, cfg, fr)
	fr.onRun = func(Cmd) error { return os.MkdirAll(b.path("photon", "photon_data"), 0o755) }
	if err := b.Photon(context.Background()); err != nil {
		t.Fatal(err)
	}
	line := fr.joined()[0]
	for _, want := range []string{"photon-tools import", "-data-dir /photon", "-host mapsvc-nominatim-candidate", "-port 5432", "-user nominatim", "-languages ru,kk,en"} {
		if !strings.Contains(line, want) {
			t.Errorf("photon command lacks %q: %s", want, line)
		}
	}
	if fi, err := os.Stat(b.path("photon")); err != nil || fi.Mode().Perm() != 0o777 {
		t.Errorf("photon dir must be world-writable for the unprivileged container user: %v %v", fi, err)
	}
	if got := fr.runs[0].String(); strings.Contains(got, "s3cret") {
		t.Errorf("password leaked into the logged command: %s", got)
	}
}

func TestDiscardRemovesCandidateAndDir(t *testing.T) {
	cfg := testConfig(t)
	fr := &fakeRunner{}
	b := newTestBuild(t, cfg, fr)
	_ = os.WriteFile(b.path("city.osm.pbf"), []byte("x"), 0o644)
	b.Discard(context.Background())
	if _, err := os.Stat(b.Dir); err == nil {
		t.Error("build dir must be removed")
	}
	var cmds []string
	for _, c := range fr.outs {
		cmds = append(cmds, strings.Join(c.Args, " "))
	}
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "rm -f -v mapsvc-nominatim-candidate") || !strings.Contains(joined, "volume rm -f mapsvc-nominatim-pg-"+b.Version) {
		t.Errorf("candidate cleanup missing:\n%s", joined)
	}
}

func TestDiscardNeverDeletesLiveVersion(t *testing.T) {
	cfg := testConfig(t)
	b := newTestBuild(t, cfg, &fakeRunner{})
	if err := dataset.SwitchCurrent(cfg.DataDir, b.Version); err != nil {
		t.Fatal(err)
	}
	b.Discard(context.Background())
	if _, err := os.Stat(b.Dir); err != nil {
		t.Error("the live version directory must survive Discard")
	}
}

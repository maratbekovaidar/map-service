package pipeline

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"map-service/internal/dataset"
)

const buildV = "20261015T030000Z" // the version the faked clock produces

// toolRunner emulates the external tools: it produces the files the real
// tools would, and can be told to fail on a command containing failOn.
type toolRunner struct {
	*fakeRunner
	st     *fakeStack
	cfg    *Config
	failOn string
}

func newToolRunner(t *testing.T, cfg *Config, st *fakeStack) *toolRunner {
	t.Helper()
	tr := &toolRunner{fakeRunner: st.runner(), st: st, cfg: cfg}
	baseOut, baseRun := tr.onOut, tr.onRun
	tr.onOut = func(c Cmd) (string, error) {
		if len(c.Args) > 0 && c.Name == cfg.OsmiumBin && c.Args[0] == "fileinfo" {
			if c.Args[1] == "-g" {
				return "2026-10-13T20:21:31Z\n", nil
			}
			return `{"data":{"count":{"nodes":832687,"ways":137473,"relations":3462}}}`, nil
		}
		return baseOut(c)
	}
	tr.onRun = func(c Cmd) error {
		line := c.Name + " " + strings.Join(c.Args, " ")
		if tr.failOn != "" && strings.Contains(line, tr.failOn) {
			return errors.New("injected failure: " + tr.failOn)
		}
		ver := c.Env["PIPELINE_VERSION"]
		dir := filepath.Join(cfg.DataDir, ver)
		switch {
		case c.Name == cfg.OsmiumBin && len(c.Args) > 0 && c.Args[0] == "extract":
			out := c.Args[indexOf(c.Args, "-o")+1]
			_ = os.MkdirAll(filepath.Dir(out), 0o755)
			if err := os.WriteFile(out, []byte("pbf"), 0o644); err != nil {
				return err
			}
		case strings.Contains(line, "osrm-tools osrm-extract"):
			_ = os.WriteFile(filepath.Join(dir, "osrm", c.Env["OSRM_PROFILE"], "city.osrm"), []byte("x"), 0o644)
		case strings.Contains(line, "run --rm --no-deps -T tilemaker"):
			_ = os.WriteFile(filepath.Join(dir, "tiles", "city.pmtiles"), append([]byte("PMTiles\x03"), make([]byte, 2048)...), 0o644)
		case strings.Contains(line, "photon-tools import"):
			_ = os.MkdirAll(filepath.Join(dir, "photon", "photon_data"), 0o755)
		}
		return baseRun(c)
	}
	return tr
}

func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}

// pipelineEnv is a data dir with a live version plus a fully faked stack.
type pipelineEnv struct {
	p     *Pipeline
	cfg   *Config
	st    *fakeStack
	tr    *toolRunner
	smoke *smokeEnv
}

func newPipelineEnv(t *testing.T) *pipelineEnv {
	t.Helper()
	st := newFakeStack(t, "osrm-car", "osrm-foot", "photon", "nominatim", "gateway")
	st.gwVer.Store(buildV)
	cfg := testConfig(t)
	cfg.GatewayURL = st.gw.URL
	cfg.SwitchTimeout = 300 * time.Millisecond
	cfg.NominatimImportTimeout = time.Second
	src := filepath.Join(t.TempDir(), "kz.osm.pbf")
	if err := os.WriteFile(src, []byte("country"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.SourceURL = "file://" + src
	cfg.SmokeGeocodeQuery = "Хан Шатыр"
	cfg.SmokeGeocodeMinResults = 1

	// A live version that must survive everything.
	if err := os.MkdirAll(dataset.Dir(cfg.DataDir, oldV), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, oldV, "city.osm.pbf"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = dataset.WriteManifest(cfg.DataDir, dataset.Manifest{Version: oldV})
	if err := dataset.SwitchCurrent(cfg.DataDir, oldV); err != nil {
		t.Fatal(err)
	}

	tr := newToolRunner(t, cfg, st)
	sm := newSmokeEnv()
	probes := sm.probe
	p := &Pipeline{
		Cfg: cfg, Log: testLogger(), Run: tr, HTTP: http.DefaultClient,
		Now:    func() time.Time { return time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC) },
		Poll:   time.Millisecond,
		Probes: &probes,
	}
	return &pipelineEnv{p: p, cfg: cfg, st: st, tr: tr, smoke: sm}
}

func (e *pipelineEnv) current(t *testing.T) string {
	t.Helper()
	v, err := dataset.Current(e.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *pipelineEnv) has(v string) bool {
	_, err := os.Stat(dataset.Dir(e.cfg.DataDir, v))
	return err == nil
}

func TestUpdateFullRun(t *testing.T) {
	e := newPipelineEnv(t)
	v, err := e.p.Update(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v != buildV || e.current(t) != buildV {
		t.Fatalf("version=%q current=%q, want %q", v, e.current(t), buildV)
	}
	// The previous version stays (KeepVersions=2), the new one is complete.
	if !e.has(oldV) || !dataset.Complete(e.cfg.DataDir, buildV) {
		t.Errorf("old kept=%v new complete=%v", e.has(oldV), dataset.Complete(e.cfg.DataDir, buildV))
	}
	m, _ := dataset.ReadManifest(e.cfg.DataDir, buildV)
	var names []string
	for _, s := range m.Steps {
		names = append(names, s.Name)
	}
	// The manifest is written inside the release step, so it lists the steps
	// that finished before it (release and cleanup themselves are not in it).
	want := "prepare,download,extract,osrm,tiles,nominatim,photon,smoke"
	if strings.Join(names, ",") != want {
		t.Errorf("steps in manifest: %s\nwant                  %s", strings.Join(names, ","), want)
	}
	if m.Source.OSMTimestamp != "2026-10-13T20:21:31Z" {
		t.Errorf("manifest source: %+v", m.Source)
	}
	// The data actually produced.
	for _, f := range []string{"city.osm.pbf", "osrm/car/city.osrm", "osrm/foot/city.osrm", "tiles/city.pmtiles", "photon/photon_data"} {
		if _, err := os.Stat(filepath.Join(e.cfg.DataDir, buildV, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	// The running services were moved to the new data.
	if got := strings.Join(e.st.recreated(), ","); got != buildV+":osrm-car,"+buildV+":osrm-foot,"+buildV+":photon,"+buildV+":nominatim" {
		t.Errorf("recreated: %s", got)
	}
}

func TestUpdateKeepsOnlyConfiguredNumberOfVersions(t *testing.T) {
	e := newPipelineEnv(t)
	e.cfg.KeepVersions = 1
	if _, err := e.p.Update(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if e.has(oldV) {
		t.Error("with DATA_KEEP_VERSIONS=1 the previous version must be removed")
	}
	if e.current(t) != buildV {
		t.Errorf("current = %s", e.current(t))
	}
}

// Acceptance criterion: a deliberately broken step must leave the live
// version serving, clean up after itself and make the updater report failure.
func TestUpdateFailureNeverTouchesTheLiveVersion(t *testing.T) {
	steps := []struct {
		name   string
		inject func(e *pipelineEnv)
	}{
		{"prepare", func(e *pipelineEnv) { e.tr.failOn = "pull --quiet" }},
		{"download", func(e *pipelineEnv) { e.cfg.SourceURL = "file:///no/such/country.osm.pbf" }},
		{"extract", func(e *pipelineEnv) { e.tr.failOn = "osmium extract" }},
		{"osrm", func(e *pipelineEnv) { e.tr.failOn = "osrm-partition" }},
		{"tiles", func(e *pipelineEnv) { e.tr.failOn = "-T tilemaker" }},
		{"nominatim", func(e *pipelineEnv) { e.tr.failOn = "up -d --no-deps nominatim-candidate" }},
		{"photon", func(e *pipelineEnv) { e.tr.failOn = "photon-tools import" }},
		{"smoke", func(e *pipelineEnv) { e.smoke.nom.search = nil }},
		{"release", func(e *pipelineEnv) { e.st.unhealthyOn["photon"] = buildV }},
	}
	for _, c := range steps {
		t.Run(c.name, func(t *testing.T) {
			e := newPipelineEnv(t)
			c.inject(e)
			_, err := e.p.Update(context.Background(), Options{})
			if err == nil {
				t.Fatal("Update must fail")
			}
			if !strings.Contains(err.Error(), `step "`+c.name+`" failed`) {
				t.Errorf("error should name the step: %v", err)
			}
			if e.current(t) != oldV {
				t.Errorf("the live version changed to %s", e.current(t))
			}
			if !e.has(oldV) {
				t.Error("the live version directory vanished")
			}
			if e.has(buildV) {
				t.Error("the failed build directory must be removed")
			}
			// Nothing the failed build created may linger: candidate container and volume are removed.
			var cleaned []string
			for _, o := range e.tr.outs {
				cleaned = append(cleaned, strings.Join(o.Args, " "))
			}
			all := strings.Join(cleaned, "\n")
			for _, want := range []string{"rm -f -v mapsvc-nominatim-candidate", "volume rm -f mapsvc-nominatim-pg-" + buildV} {
				if !strings.Contains(all, want) {
					t.Errorf("cleanup command %q not issued:\n%s", want, all)
				}
			}
		})
	}
}

func TestUpdateReleaseFailureRestoresOldServices(t *testing.T) {
	e := newPipelineEnv(t)
	e.st.unhealthyOn["nominatim"] = buildV
	if _, err := e.p.Update(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	re := e.st.recreated()
	if last := re[len(re)-1]; last != oldV+":nominatim" {
		t.Errorf("the last recreation must put nominatim back on %s, got %v", oldV, re)
	}
}

func TestUpdateRefusesConcurrentRuns(t *testing.T) {
	e := newPipelineEnv(t)
	unlock, err := lockDir(e.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := e.p.Update(context.Background(), Options{}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	if len(e.tr.runs) != 0 {
		t.Error("a second updater must not start any work")
	}
	unlock()
	if _, err := e.p.Update(context.Background(), Options{}); err != nil {
		t.Errorf("after the lock is released the update must run: %v", err)
	}
}

func TestUpdateOnlyModeNeverSwitchesAndKeepsTheBuild(t *testing.T) {
	e := newPipelineEnv(t)
	v, err := e.p.Update(context.Background(), Options{Only: []string{"download", "extract"}})
	if err != nil {
		t.Fatal(err)
	}
	if e.current(t) != oldV {
		t.Error("a partial run must never switch the live version")
	}
	if !e.has(v) {
		t.Error("the partial build directory must be kept for inspection")
	}
	if _, err := e.p.Update(context.Background(), Options{Only: []string{"extract", "bogus"}}); err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("unknown step: %v", err)
	}
	// A failure in -only mode also keeps the directory.
	e2 := newPipelineEnv(t)
	e2.tr.failOn = "osmium extract"
	if _, err := e2.p.Update(context.Background(), Options{Only: []string{"download", "extract"}}); err == nil {
		t.Fatal("expected failure")
	}
	if !e2.has(buildV) {
		t.Error("-only keeps the build directory even after a failure")
	}
}

func TestUpdateResumeExistingVersion(t *testing.T) {
	e := newPipelineEnv(t)
	if _, err := e.p.Update(context.Background(), Options{Only: []string{"download", "extract"}}); err != nil {
		t.Fatal(err)
	}
	// The faked clock would pick the same version anyway; use an explicit one to prove the option.
	custom := "20260101T000000Z"
	if err := os.Rename(filepath.Join(e.cfg.DataDir, buildV), filepath.Join(e.cfg.DataDir, custom)); err != nil {
		t.Fatal(err)
	}
	v, err := e.p.Update(context.Background(), Options{Only: []string{"osrm"}, Version: custom})
	if err != nil || v != custom {
		t.Fatalf("v=%q err=%v", v, err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, custom, "osrm", "car", "city.osrm")); err != nil {
		t.Errorf("the OSRM step must have run inside the resumed directory: %v", err)
	}
	if _, err := e.p.Update(context.Background(), Options{Version: "20250101T000000Z"}); err == nil {
		t.Error("resuming a version that does not exist must fail")
	}
	if _, err := e.p.Update(context.Background(), Options{Version: "../etc"}); err == nil {
		t.Error("an invalid version must be rejected")
	}
}

func TestUpdateCancellationStillCleansUp(t *testing.T) {
	e := newPipelineEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.tr.onOut = nil // keep the scripted outputs out of the way
	prev := e.tr.onRun
	e.tr.onRun = func(c Cmd) error {
		if strings.Contains(strings.Join(c.Args, " "), "osrm-partition") {
			cancel() // SIGTERM arrives mid-build
			return context.Canceled
		}
		return prev(c)
	}
	e.tr.onOut = func(c Cmd) (string, error) {
		if c.Name == e.cfg.OsmiumBin && c.Args[0] == "fileinfo" {
			if c.Args[1] == "-g" {
				return "2026-10-13T20:21:31Z\n", nil
			}
			return `{"data":{"count":{"nodes":832687,"ways":137473,"relations":3462}}}`, nil
		}
		return "", nil
	}
	if _, err := e.p.Update(ctx, Options{}); err == nil {
		t.Fatal("expected an error")
	}
	if e.has(buildV) {
		t.Error("a cancelled build must still be cleaned up (cleanup uses a fresh context)")
	}
	if e.current(t) != oldV {
		t.Error("the live version must be untouched")
	}
}

package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"map-service/internal/dataset"
)

const (
	oldV = "20261001T030000Z"
	newV = "20261008T030000Z"
)

// fakeStack scripts the docker answers the release step relies on.
type fakeStack struct {
	deployed map[string]bool // service -> its container exists
	// unhealthyOn maps a service to the data version on which it never becomes
	// healthy (e.g. the new data breaks it); other versions are fine.
	unhealthyOn map[string]string
	lastUp      map[string]string // service -> DATA_VERSION of its latest (re)creation
	failUp      map[string]bool   // service whose `up --force-recreate` fails
	gw          *httptest.Server
	gwVer       atomic.Value // data_version the fake gateway reports
	gwStatus    atomic.Int32
	log         []string // every command, in order, annotated with DATA_VERSION for compose
}

func newFakeStack(t *testing.T, deployed ...string) *fakeStack {
	t.Helper()
	s := &fakeStack{deployed: map[string]bool{}, unhealthyOn: map[string]string{}, lastUp: map[string]string{}, failUp: map[string]bool{}}
	for _, d := range deployed {
		s.deployed[d] = true
	}
	s.gwStatus.Store(http.StatusOK)
	s.gw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ver, _ := s.gwVer.Load().(string)
		w.WriteHeader(int(s.gwStatus.Load()))
		fmt.Fprintf(w, `{"status":"ok","data_version":%q}`, ver)
	}))
	t.Cleanup(s.gw.Close)
	return s
}

func (s *fakeStack) runner() *fakeRunner {
	fr := &fakeRunner{}
	fr.onOut = func(c Cmd) (string, error) {
		line := strings.Join(c.Args, " ")
		s.log = append(s.log, "OUT "+line)
		switch {
		case strings.Contains(line, " ps -a -q "):
			svc := c.Args[len(c.Args)-1]
			if s.deployed[svc] {
				return "id-" + svc + "\n", nil
			}
			return "", nil
		case c.Args[0] == "inspect":
			svc := strings.TrimPrefix(c.Args[len(c.Args)-1], "id-")
			if v, bad := s.unhealthyOn[svc]; bad && v == s.lastUp[svc] {
				return "running unhealthy\n", nil
			}
			return "running healthy\n", nil
		case c.Args[0] == "logs":
			return "service crashed: out of memory\n", nil
		}
		return "", nil
	}
	fr.onRun = func(c Cmd) error {
		line := strings.Join(c.Args, " ")
		s.log = append(s.log, fmt.Sprintf("RUN[DATA_VERSION=%s] %s", c.Env["DATA_VERSION"], line))
		if strings.Contains(line, "up -d --no-deps --force-recreate") {
			svc := c.Args[len(c.Args)-1]
			s.lastUp[svc] = c.Env["DATA_VERSION"]
			if s.failUp[svc] && c.Env["DATA_VERSION"] == newV {
				return fmt.Errorf("docker: cannot recreate %s", svc)
			}
		}
		return nil
	}
	return fr
}

// recreated lists the services recreated, with the data version they were started on.
func (s *fakeStack) recreated() []string {
	var out []string
	for _, l := range s.log {
		if strings.HasPrefix(l, "RUN[") && strings.Contains(l, "--force-recreate") {
			ver := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(l, "]", 2)[0], "RUN[DATA_VERSION="), "")
			out = append(out, ver+":"+l[strings.LastIndex(l, " ")+1:])
		}
	}
	return out
}

// releaseBuild prepares data/<oldV> (live) and data/<newV> (being released).
func releaseBuild(t *testing.T, st *fakeStack, withOld bool) *Build {
	t.Helper()
	cfg := testConfig(t)
	cfg.GatewayURL = st.gw.URL
	cfg.SwitchTimeout = 300 * time.Millisecond
	b := newTestBuild(t, cfg, st.runner())
	b.Version = newV
	b.Dir = dataset.Dir(cfg.DataDir, newV)
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withOld {
		if err := os.MkdirAll(dataset.Dir(cfg.DataDir, oldV), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = dataset.WriteManifest(cfg.DataDir, dataset.Manifest{Version: oldV})
		if err := dataset.SwitchCurrent(cfg.DataDir, oldV); err != nil {
			t.Fatal(err)
		}
	}
	b.PollInterval = time.Millisecond
	b.steps = []dataset.Step{{Name: "osrm", DurationS: 1.5}}
	return b
}

func TestReleaseSwitchesAndRestartsServicesOneByOne(t *testing.T) {
	st := newFakeStack(t, "osrm-car", "osrm-foot", "photon", "nominatim", "gateway")
	st.gwVer.Store(newV)
	b := releaseBuild(t, st, true)

	if err := b.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cur, _ := dataset.Current(b.Cfg.DataDir); cur != newV {
		t.Errorf("current = %q, want %q", cur, newV)
	}
	// The manifest marks the version complete and records the build.
	m, err := dataset.ReadManifest(b.Cfg.DataDir, newV)
	if err != nil || m.City != "Astana" || len(m.Steps) != 1 || m.Steps[0].Name != "osrm" || m.BBox != "71.04,50.74,71.96,51.46" {
		t.Errorf("manifest: %+v %v", m, err)
	}
	// Order: OSRM first (fast), then photon, then nominatim - each on the NEW data version.
	want := []string{newV + ":osrm-car", newV + ":osrm-foot", newV + ":photon", newV + ":nominatim"}
	if got := st.recreated(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recreated = %v\nwant       %v", got, want)
	}
	// The gateway is never restarted: it follows the symlink by itself.
	for _, l := range st.log {
		if strings.Contains(l, "--force-recreate gateway") {
			t.Error("the gateway must not be restarted")
		}
	}
	// The candidate Nominatim was stopped (gracefully) BEFORE its volume went live.
	var stopAt, upNominatimAt = -1, -1
	for i, l := range st.log {
		if strings.Contains(l, "stop --time 90 mapsvc-nominatim-candidate") {
			stopAt = i
		}
		if strings.Contains(l, "--force-recreate nominatim") {
			upNominatimAt = i
		}
	}
	if stopAt < 0 || upNominatimAt < 0 || stopAt > upNominatimAt {
		t.Errorf("candidate must be stopped before nominatim is recreated (stop=%d up=%d)", stopAt, upNominatimAt)
	}
}

func TestReleaseWithoutRunningStackOnlyFlipsTheLink(t *testing.T) {
	st := newFakeStack(t) // nothing deployed
	b := releaseBuild(t, st, false)
	if err := b.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cur, _ := dataset.Current(b.Cfg.DataDir); cur != newV {
		t.Errorf("current = %q", cur)
	}
	if got := st.recreated(); len(got) != 0 {
		t.Errorf("nothing to restart, got %v", got)
	}
}

func TestReleaseOnlyRestartsServicesThatExist(t *testing.T) {
	st := newFakeStack(t, "osrm-car", "nominatim") // no foot, no photon, no gateway
	b := releaseBuild(t, st, true)
	if err := b.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.recreated(), ","); got != newV+":osrm-car,"+newV+":nominatim" {
		t.Errorf("recreated = %s", got)
	}
}

func TestReleaseRollsBackWhenAServiceStaysUnhealthy(t *testing.T) {
	st := newFakeStack(t, "osrm-car", "osrm-foot", "photon", "nominatim", "gateway")
	st.gwVer.Store(newV)
	st.unhealthyOn["photon"] = newV
	b := releaseBuild(t, st, true)

	err := b.Release(context.Background())
	if err == nil {
		t.Fatal("a failing service must fail the release")
	}
	for _, want := range []string{"photon", "rolled back to " + oldV} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if cur, _ := dataset.Current(b.Cfg.DataDir); cur != oldV {
		t.Errorf("current must be restored to %s, got %q", oldV, cur)
	}
	// The services that were already switched are recreated again on the OLD version.
	got := strings.Join(st.recreated(), ",")
	if !strings.Contains(got, oldV+":osrm-car") || !strings.Contains(got, oldV+":photon") {
		t.Errorf("rollback must recreate services on %s: %s", oldV, got)
	}
	if strings.Contains(strings.Join(st.recreated()[len(st.recreated())-2:], ","), newV) {
		t.Errorf("after the rollback services must run %s: %v", oldV, st.recreated())
	}
}

func TestReleaseRollsBackWhenRecreateFails(t *testing.T) {
	st := newFakeStack(t, "osrm-car", "photon", "nominatim")
	st.failUp["osrm-car"] = true
	b := releaseBuild(t, st, true)
	err := b.Release(context.Background())
	if err == nil || !strings.Contains(err.Error(), "osrm-car") {
		t.Fatalf("err = %v", err)
	}
	if cur, _ := dataset.Current(b.Cfg.DataDir); cur != oldV {
		t.Errorf("current = %q, want %q", cur, oldV)
	}
}

func TestReleaseRollsBackWhenGatewayDoesNotSeeTheNewVersion(t *testing.T) {
	st := newFakeStack(t, "osrm-car", "gateway")
	st.gwVer.Store(oldV) // the gateway keeps reporting the old version
	b := releaseBuild(t, st, true)
	err := b.Release(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if cur, _ := dataset.Current(b.Cfg.DataDir); cur != oldV {
		t.Errorf("current = %q", cur)
	}
}

func TestReleaseRollsBackWhenGatewayIsNotReady(t *testing.T) {
	st := newFakeStack(t, "osrm-car", "gateway")
	st.gwVer.Store(newV)
	st.gwStatus.Store(http.StatusServiceUnavailable)
	b := releaseBuild(t, st, true)
	if err := b.Release(context.Background()); err == nil || !strings.Contains(err.Error(), "readyz") {
		t.Fatalf("err = %v", err)
	}
}

func TestFirstDeploymentCannotRollBack(t *testing.T) {
	st := newFakeStack(t, "nominatim")
	st.unhealthyOn["nominatim"] = newV
	b := releaseBuild(t, st, false)
	err := b.Release(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no previous version") {
		t.Fatalf("err = %v", err)
	}
}

// ------------------------------------------------------------------ cleanup

func cleanupBuild(t *testing.T, keep int, versions map[string]bool, current string) (*Build, *fakeRunner) {
	t.Helper()
	cfg := testConfig(t)
	cfg.KeepVersions = keep
	fr := &fakeRunner{}
	b := newTestBuild(t, cfg, fr)
	if err := os.RemoveAll(b.Dir); err != nil { // the helper's own (empty) build dir is not part of the scenario
		t.Fatal(err)
	}
	for v, complete := range versions {
		dir := dataset.Dir(cfg.DataDir, v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "city.osm.pbf"), []byte("x"), 0o644)
		if complete {
			_ = dataset.WriteManifest(cfg.DataDir, dataset.Manifest{Version: v})
		}
	}
	if current != "" {
		if err := dataset.SwitchCurrent(cfg.DataDir, current); err != nil {
			t.Fatal(err)
		}
	}
	return b, fr
}

func exists(b *Build, v string) bool {
	_, err := os.Stat(dataset.Dir(b.Cfg.DataDir, v))
	return err == nil
}

func TestCleanupKeepsLiveAndNewestOthers(t *testing.T) {
	b, fr := cleanupBuild(t, 2, map[string]bool{
		"20260901T000000Z": true, "20260908T000000Z": true, "20260915T000000Z": true, "20260922T000000Z": true,
	}, "20260922T000000Z")
	if err := b.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"20260922T000000Z": true, "20260915T000000Z": true, "20260908T000000Z": false, "20260901T000000Z": false} {
		if exists(b, v) != want {
			t.Errorf("%s exists=%v, want %v", v, exists(b, v), want)
		}
	}
	// The Nominatim volumes of removed versions go with them - and only those.
	var vols []string
	for _, c := range fr.outs {
		if c.Args[0] == "volume" {
			vols = append(vols, c.Args[len(c.Args)-1])
		}
	}
	if strings.Join(vols, ",") != "mapsvc-nominatim-pg-20260901T000000Z,mapsvc-nominatim-pg-20260908T000000Z" {
		t.Errorf("volumes removed: %v", vols)
	}
}

func TestCleanupRemovesCrashedBuildsAndRespectsKeepOne(t *testing.T) {
	b, _ := cleanupBuild(t, 1, map[string]bool{
		"20260915T000000Z": true,
		"20260922T000000Z": true,
		"20260925T000000Z": false, // a crashed build, newer than the live version
		"20260801T000000Z": false, // an ancient crashed build
	}, "20260922T000000Z")
	if err := b.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(b, "20260922T000000Z") {
		t.Error("the live version must never be removed")
	}
	for _, v := range []string{"20260915T000000Z", "20260925T000000Z", "20260801T000000Z"} {
		if exists(b, v) {
			t.Errorf("%s should have been removed", v)
		}
	}
}

func TestCleanupToleratesDockerErrors(t *testing.T) {
	b, fr := cleanupBuild(t, 1, map[string]bool{"20260915T000000Z": true, "20260922T000000Z": true}, "20260922T000000Z")
	fr.onOut = func(Cmd) (string, error) { return "", fmt.Errorf("volume is in use") }
	if err := b.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup runs after a successful switch and must not fail it: %v", err)
	}
	if exists(b, "20260915T000000Z") {
		t.Error("the directory is removed even if the volume could not be")
	}
}

// ----------------------------------------------------------------- schedule

func TestNextRun(t *testing.T) {
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) // a Wednesday
	cases := []struct {
		spec string
		want time.Time
	}{
		{"0 3 * * 0", time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)}, // next Sunday 03:00 UTC
		{"30 2 * * *", time.Date(2026, 10, 15, 2, 30, 0, 0, time.UTC)},
		{"@weekly", time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)},
		{"CRON_TZ=Asia/Almaty 0 3 * * *", time.Date(2026, 10, 14, 22, 0, 0, 0, time.UTC)}, // 03:00 at +05:00
	}
	for _, c := range cases {
		got, err := NextRun(c.spec, base)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("NextRun(%q) = %v, %v; want %v", c.spec, got, err, c.want)
		}
	}
	// Interpreted in UTC even when the process runs in another zone.
	local := base.In(time.FixedZone("X", 9*3600))
	if got, _ := NextRun("0 3 * * 0", local); !got.Equal(time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("zone-independent: %v", got)
	}
	for _, bad := range []string{"", "not cron", "61 * * * *", "* * * *"} {
		if _, err := NextRun(bad, base); err == nil {
			t.Errorf("NextRun(%q) must fail", bad)
		}
	}
}

func TestRunCronRunsAtEachFiringAndSurvivesFailures(t *testing.T) {
	cfg := testConfig(t)
	cfg.Cron = "0 3 * * *"
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	updates := 0
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pipeline{Cfg: cfg, Log: testLogger(), Run: &fakeRunner{}, HTTP: http.DefaultClient, Now: func() time.Time { return now }}
	p.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		now = now.Add(d) // time advances to the firing
		if len(sleeps) == 4 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	// Every update fails immediately (empty source); the loop must carry on.
	p.Run = &fakeRunner{onRun: func(Cmd) error { updates++; return fmt.Errorf("boom") }}
	cfg.SourceURL = "file:///does/not/exist"

	if err := p.RunCron(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 4 {
		t.Fatalf("sleeps = %v", sleeps)
	}
	if sleeps[0] != 15*time.Hour { // 12:00 -> 03:00 next day
		t.Errorf("first wait = %v, want 15h", sleeps[0])
	}
	for i, d := range sleeps[1:3] {
		if d != 24*time.Hour {
			t.Errorf("wait %d = %v, want 24h", i+1, d)
		}
	}
}

func TestRunCronRejectsBadSpecEarly(t *testing.T) {
	cfg := testConfig(t)
	cfg.Cron = "whenever"
	p := &Pipeline{Cfg: cfg, Log: testLogger(), Now: time.Now}
	if err := p.RunCron(context.Background()); err == nil {
		t.Fatal("an invalid cron expression must be reported immediately")
	}
}

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"map-service/internal/dataset"
)

// Build is one run of the pipeline: it owns data/<Version> and the candidate
// containers created for it.
type Build struct {
	Cfg     *Config
	Version string
	Dir     string // data/<Version>
	Log     *slog.Logger
	Run     Runner
	HTTP    *http.Client

	// PollInterval is how often container health is polled (tests shorten it).
	PollInterval time.Duration

	// Probes are the clients used by the smoke tests.
	Probes Probes
	// CandidateTimeout bounds how long a smoke-test candidate may take to start.
	CandidateTimeout time.Duration

	sourcePath string
	source     dataset.Source
	steps      []dataset.Step
}

// record remembers a finished step for the manifest.
func (b *Build) record(name string, d time.Duration) {
	b.steps = append(b.steps, dataset.Step{Name: name, DurationS: d.Round(100 * time.Millisecond).Seconds()})
}

// NewBuild prepares a build for version.
func NewBuild(cfg *Config, version string, log *slog.Logger, run Runner, hc *http.Client) *Build {
	return &Build{
		Cfg:          cfg,
		Version:      version,
		Dir:          dataset.Dir(cfg.DataDir, version),
		Log:          log,
		Run:          run,
		HTTP:         hc,
		PollInterval: 10 * time.Second,
		Probes:       DefaultProbes(log, cfg.DataDir),

		CandidateTimeout: defaultCandidateTimeout,
	}
}

func (b *Build) path(parts ...string) string {
	return filepath.Join(append([]string{b.Dir}, parts...)...)
}

// candidateContainer is the name of the Nominatim container for this build
// (must match container_name in deploy/docker-compose.yml).
func (b *Build) candidateContainer() string { return b.Cfg.Project + "-nominatim-candidate" }

// candidateVolume is the named volume holding the new PostgreSQL data (must
// match the name of the nominatim-pg-candidate volume in the compose file).
func (b *Build) candidateVolume() string {
	return b.Cfg.Project + "-nominatim-pg-" + b.Version
}

// ----------------------------------------------------------------- compose

func (b *Build) composeEnv(extra map[string]string) map[string]string {
	live := "none"
	if v, err := dataset.Current(b.Cfg.DataDir); err == nil {
		live = v
	}
	env := map[string]string{
		"DATA_DIR":         b.Cfg.DataDir,
		"REPO_DIR":         b.Cfg.RepoDir,
		"DATA_VERSION":     live,
		"PIPELINE_VERSION": b.Version,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func (b *Build) composeCmd(extra map[string]string, args ...string) Cmd {
	full := []string{"compose", "-p", b.Cfg.Project, "-f", b.Cfg.ComposeFile}
	if _, err := os.Stat(b.Cfg.EnvFile); err == nil {
		full = append(full, "--env-file", b.Cfg.EnvFile)
	}
	full = append(full, args...)
	return Cmd{Name: b.Cfg.DockerBin, Args: full, Env: b.composeEnv(extra)}
}

func (b *Build) compose(ctx context.Context, extra map[string]string, args ...string) error {
	return b.Run.Run(ctx, b.Log, b.composeCmd(extra, args...))
}

// composeRun runs a one-off tool service and removes the container.
func (b *Build) composeRun(ctx context.Context, extra map[string]string, service string, args ...string) error {
	a := append([]string{"run", "--rm", "--no-deps", "-T", service}, args...)
	return b.compose(ctx, extra, a...)
}

// ----------------------------------------------------------------- prepare

// Prepare builds/pulls the images the later steps need so that downloads do
// not count against (and cannot stall) the data steps.
func (b *Build) Prepare(ctx context.Context) error {
	if err := b.compose(ctx, nil, "build", "photon"); err != nil {
		return fmt.Errorf("build photon image: %w", err)
	}
	if err := b.compose(ctx, nil, "pull", "--quiet", "osrm-tools", "tilemaker", "nominatim-candidate"); err != nil {
		return fmt.Errorf("pull images: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- download

// Download makes the country extract available locally. http(s) sources are
// cached in data/cache with ETag revalidation; file sources are used in place.
func (b *Build) Download(ctx context.Context) error {
	kind, loc, err := ParseSource(b.Cfg.SourceURL)
	if err != nil {
		return err
	}
	path := loc
	if kind == sourceHTTP {
		path = filepath.Join(b.Cfg.DataDir, "cache", "source.osm.pbf")
		if err := b.fetch(ctx, loc, path); err != nil {
			return fmt.Errorf("download %s: %w", loc, err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("source extract: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return fmt.Errorf("source extract %s is not a non-empty regular file", path)
	}
	b.sourcePath = path
	b.source = dataset.Source{URL: b.Cfg.SourceURL, SizeBytes: fi.Size()}
	if out, err := b.Run.Output(ctx, Cmd{Name: b.Cfg.OsmiumBin, Args: []string{"fileinfo", "-g", "header.option.timestamp", path}}); err == nil {
		b.source.OSMTimestamp = strings.TrimSpace(out)
	} else {
		b.Log.Warn("cannot read source timestamp", "err", err)
	}
	b.Log.Info("source ready", "path", path, "size_bytes", fi.Size(), "osm_timestamp", b.source.OSMTimestamp)
	return nil
}

// downloadIdleTimeout aborts a transfer that stops making progress.
const downloadIdleTimeout = 2 * time.Minute

func (b *Build) fetch(ctx context.Context, rawURL, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(downloadIdleTimeout, cancel)
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "map-service-updater/1.0")
	etagFile := dest + ".etag"
	if _, err := os.Stat(dest); err == nil {
		if et, err := os.ReadFile(etagFile); err == nil && len(et) > 0 {
			req.Header.Set("If-None-Match", strings.TrimSpace(string(et)))
		}
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		b.Log.Info("source not modified, reusing cached extract", "path", dest)
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	pr := &progressWriter{log: b.Log, total: resp.ContentLength, idle: idle, last: time.Now()}
	n, copyErr := io.Copy(io.MultiWriter(f, pr), resp.Body)
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		copyErr = fmt.Errorf("truncated download: got %d of %d bytes", n, resp.ContentLength)
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	if et := resp.Header.Get("ETag"); et != "" {
		_ = os.WriteFile(etagFile, []byte(et), 0o644)
	} else {
		_ = os.Remove(etagFile)
	}
	b.Log.Info("download complete", "path", dest, "bytes", n)
	return nil
}

// progressWriter logs progress and keeps the idle watchdog alive.
type progressWriter struct {
	log   *slog.Logger
	total int64
	done  int64
	idle  *time.Timer
	last  time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	p.idle.Reset(downloadIdleTimeout)
	if time.Since(p.last) > 10*time.Second {
		p.last = time.Now()
		if p.total > 0 {
			p.log.Info("downloading", "mb", p.done>>20, "of_mb", p.total>>20)
		} else {
			p.log.Info("downloading", "mb", p.done>>20)
		}
	}
	return len(b), nil
}

// ----------------------------------------------------------------- extract

// Extract cuts the city out of the country extract into data/<v>/city.osm.pbf.
func (b *Build) Extract(ctx context.Context) error {
	if b.sourcePath == "" {
		return errors.New("extract: no source extract (download step did not run)")
	}
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return err
	}
	out := b.path(dataset.PBFFile)
	args := []string{"extract", "--strategy", b.Cfg.ExtractStrategy}
	if b.Cfg.ExtractStrategy == "smart" {
		// Keep whole multipolygons (lakes, landuse) and boundaries.
		args = append(args, "-S", "types=multipolygon,boundary")
	}
	switch b.Cfg.ExtractMode {
	case ExtractBBox:
		args = append(args, "--bbox", b.Cfg.BBox.String())
	case ExtractPolygon:
		if _, err := os.Stat(b.Cfg.PolygonFile); err != nil {
			return fmt.Errorf("polygon file: %w", err)
		}
		args = append(args, "--polygon", b.Cfg.PolygonFile)
	default:
		return fmt.Errorf("unknown extract mode %q", b.Cfg.ExtractMode)
	}
	args = append(args, "--set-bounds", "--overwrite", "-o", out, b.sourcePath)
	if err := b.Run.Run(ctx, b.Log, Cmd{Name: b.Cfg.OsmiumBin, Args: args}); err != nil {
		return fmt.Errorf("osmium extract: %w", err)
	}
	return b.checkExtract(ctx, out)
}

type fileInfo struct {
	Data struct {
		Count struct {
			Nodes     int64 `json:"nodes"`
			Ways      int64 `json:"ways"`
			Relations int64 `json:"relations"`
		} `json:"count"`
	} `json:"data"`
}

// Minimum object counts below which the extract is considered broken (wrong
// bbox, wrong polygon, source that does not cover the city).
const (
	minExtractNodes = 1000
	minExtractWays  = 100
)

func (b *Build) checkExtract(ctx context.Context, pbf string) error {
	out, err := b.Run.Output(ctx, Cmd{Name: b.Cfg.OsmiumBin, Args: []string{"fileinfo", "-e", "-j", pbf}})
	if err != nil {
		return fmt.Errorf("osmium fileinfo: %w", err)
	}
	var fi fileInfo
	if err := json.Unmarshal([]byte(out), &fi); err != nil {
		return fmt.Errorf("parse osmium fileinfo: %w", err)
	}
	c := fi.Data.Count
	b.Log.Info("city extract", "nodes", c.Nodes, "ways", c.Ways, "relations", c.Relations)
	if c.Nodes < minExtractNodes || c.Ways < minExtractWays {
		return fmt.Errorf("city extract looks empty (%d nodes, %d ways): check CITY_BBOX/CITY_POLYGON_FILE and OSM_SOURCE_URL", c.Nodes, c.Ways)
	}
	return nil
}

// -------------------------------------------------------------------- OSRM

// OSRM builds a routing graph (extract -> partition -> customize, MLD) for
// every profile in ROUTING_PROFILES, one after another to bound memory use.
func (b *Build) OSRM(ctx context.Context) error {
	for _, profile := range b.Cfg.Profiles {
		if err := b.osrmProfile(ctx, profile); err != nil {
			return fmt.Errorf("osrm %s: %w", profile, err)
		}
	}
	return nil
}

func (b *Build) osrmProfile(ctx context.Context, profile string) error {
	dir := b.path("osrm", profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// osrm-extract writes next to its input, so every profile gets its own
	// directory with a hard link (or copy) of the city extract.
	link := filepath.Join(dir, dataset.PBFFile)
	if err := linkOrCopy(b.path(dataset.PBFFile), link); err != nil {
		return err
	}
	defer os.Remove(link)

	lua := "/opt/" + profile + ".lua" // stock profile shipped in the image
	if _, err := os.Stat(filepath.Join(b.Cfg.RepoDir, "deploy", "osrm", profile+".lua")); err == nil {
		lua = "/profiles/" + profile + ".lua"
		b.Log.Info("using custom OSRM profile", "profile", profile, "lua", lua)
	}
	env := map[string]string{"OSRM_PROFILE": profile}
	for _, args := range [][]string{
		{"osrm-extract", "-p", lua, "/data/" + dataset.PBFFile},
		{"osrm-partition", "/data/city.osrm"},
		{"osrm-customize", "/data/city.osrm"},
	} {
		if err := b.composeRun(ctx, env, "osrm-tools", args...); err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "city.osrm*"))
	if len(matches) == 0 {
		return errors.New("osrm tools produced no city.osrm* files")
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ------------------------------------------------------------------- tiles

// Tiles runs tilemaker (OpenMapTiles schema) and writes data/<v>/tiles/city.pmtiles.
func (b *Build) Tiles(ctx context.Context) error {
	if err := os.MkdirAll(b.path("tiles"), 0o755); err != nil {
		return err
	}
	if err := b.writeTilemakerConfig(); err != nil {
		return err
	}
	err := b.composeRun(ctx, nil, "tilemaker",
		"--input", "/data/"+dataset.PBFFile,
		"--output", "/data/"+dataset.TilesFile,
		"--config", "/data/tilemaker.json",
		"--process", "/config/process-openmaptiles.lua",
	)
	if err != nil {
		return fmt.Errorf("tilemaker: %w", err)
	}
	return CheckPMTiles(b.path(dataset.TilesFile))
}

// writeTilemakerConfig derives the per-city tilemaker config: the committed
// template plus the city bounding box (the extract is wider than the city
// because complete boundaries/long ways are kept) and name.
func (b *Build) writeTilemakerConfig() error {
	raw, err := os.ReadFile(filepath.Join(b.Cfg.RepoDir, "deploy", "tiles", "config-openmaptiles.json"))
	if err != nil {
		return fmt.Errorf("tilemaker config template: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("tilemaker config template: %w", err)
	}
	settings, _ := cfg["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		cfg["settings"] = settings
	}
	bb := b.Cfg.BBox
	settings["bounding_box"] = []float64{bb.MinLon, bb.MinLat, bb.MaxLon, bb.MaxLat}
	settings["name"] = b.Cfg.Name + " (OpenMapTiles schema)"
	out, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile(b.path("tilemaker.json"), out, 0o644)
}

// CheckPMTiles verifies the PMTiles v3 magic and a sane size.
func CheckPMTiles(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("pmtiles: %w", err)
	}
	defer f.Close()
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fmt.Errorf("pmtiles: read header: %w", err)
	}
	if string(hdr[:7]) != "PMTiles" || hdr[7] != 3 {
		return fmt.Errorf("pmtiles: %s is not a PMTiles v3 archive", path)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() < 1024 {
		return fmt.Errorf("pmtiles: %s is suspiciously small", path)
	}
	return nil
}

// --------------------------------------------------------------- Nominatim

// Nominatim imports the city extract into a new PostgreSQL volume by starting
// the candidate container and waits until it is healthy (import finished and
// the API answers).
func (b *Build) Nominatim(ctx context.Context) error {
	if err := b.compose(ctx, nil, "up", "-d", "--no-deps", "nominatim-candidate"); err != nil {
		return fmt.Errorf("start candidate nominatim: %w", err)
	}
	if err := b.waitHealthy(ctx, b.candidateContainer(), b.Cfg.NominatimImportTimeout); err != nil {
		return fmt.Errorf("nominatim import: %w", err)
	}
	return nil
}

func (b *Build) waitHealthy(ctx context.Context, container string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := b.Run.Output(ctx, Cmd{Name: b.Cfg.DockerBin, Args: []string{
			"inspect", "-f", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", container,
		}})
		if err != nil {
			return fmt.Errorf("inspect %s: %w", container, err)
		}
		fields := strings.Fields(out)
		status, health := "", ""
		if len(fields) > 0 {
			status = fields[0]
		}
		if len(fields) > 1 {
			health = fields[1]
		}
		switch {
		case status == "exited" || status == "dead":
			return fmt.Errorf("container %s stopped unexpectedly (%s): %s", container, status, b.tailLogs(ctx, container))
		case status == "running" && health == "healthy":
			b.Log.Info("container healthy", "container", container)
			return nil
		case health == "unhealthy":
			return fmt.Errorf("container %s is unhealthy: %s", container, b.tailLogs(ctx, container))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for %s (status=%s health=%s)", timeout, container, status, health)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.PollInterval):
		}
	}
}

func (b *Build) tailLogs(ctx context.Context, container string) string {
	out, err := b.Run.Output(ctx, Cmd{Name: b.Cfg.DockerBin, Args: []string{"logs", "--tail", "15", container}})
	if err != nil {
		return "(no logs: " + err.Error() + ")"
	}
	return lastLines(out, 15)
}

// ------------------------------------------------------------------ Photon

// Photon imports the freshly built Nominatim database into a new Photon
// (embedded OpenSearch) index at data/<v>/photon.
func (b *Build) Photon(ctx context.Context) error {
	dir := b.path("photon")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	// The photon image runs as an unprivileged user; umask may have trimmed the mode.
	if err := os.Chmod(dir, 0o777); err != nil {
		return err
	}
	err := b.composeRun(ctx, nil, "photon-tools", "import",
		"-data-dir", "/photon",
		"-host", b.candidateContainer(),
		"-port", "5432",
		"-database", "nominatim",
		"-user", "nominatim",
		"-password", b.Cfg.NominatimPassword,
		"-languages", strings.Join(b.Cfg.Languages, ","),
	)
	if err != nil {
		return fmt.Errorf("photon import: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "photon_data")); err != nil {
		return fmt.Errorf("photon import produced no photon_data: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ cleanup

// Discard removes everything a failed build created: the candidate container,
// its volume and the version directory. It never touches the live version.
func (b *Build) Discard(ctx context.Context) {
	docker := b.Cfg.DockerBin
	for _, c := range []Cmd{
		{Name: docker, Args: []string{"rm", "-f", "-v", b.candidateContainer()}},
		{Name: docker, Args: []string{"volume", "rm", "-f", b.candidateVolume()}},
	} {
		if _, err := b.Run.Output(ctx, c); err != nil {
			b.Log.Warn("cleanup command failed", "cmd", c.String(), "err", err)
		}
	}
	if live, err := dataset.Current(b.Cfg.DataDir); err == nil && live == b.Version {
		b.Log.Error("refusing to delete the live version directory", "version", b.Version)
		return
	}
	if err := os.RemoveAll(b.Dir); err != nil {
		b.Log.Warn("cannot remove build directory", "dir", b.Dir, "err", err)
	}
}

// formatDuration is used for the manifest/log summaries.
func formatDuration(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}

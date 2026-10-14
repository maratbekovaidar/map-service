package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"map-service/internal/dataset"
)

// ErrAlreadyRunning is returned when another update holds the lock.
var ErrAlreadyRunning = errors.New("another update is already running")

// Step is one named stage of the pipeline.
type Step struct {
	Name string
	Run  func(ctx context.Context) error
}

// Options tune a pipeline run (mostly for debugging).
type Options struct {
	// Version resumes an existing build directory instead of starting a new
	// version (debugging, together with Only). Empty = new version.
	Version string
	// Only restricts the run to the named steps; the version is never
	// switched live and the build directory is kept. Empty = full run.
	Only []string
	// KeepOnFailure leaves the build directory and candidate containers in
	// place after a failure so they can be inspected.
	KeepOnFailure bool
}

// Pipeline builds a new data version and, when everything verified, makes it live.
type Pipeline struct {
	Cfg  *Config
	Log  *slog.Logger
	Run  Runner
	HTTP *http.Client
	Now  func() time.Time
	// Poll is the container health polling interval (tests shorten it).
	Poll time.Duration
	// Sleep waits for the next scheduled run (tests inject a fake).
	Sleep func(ctx context.Context, d time.Duration) error
	// Probes override the smoke-test clients (tests).
	Probes *Probes
}

// New returns a Pipeline with production defaults.
func New(cfg *Config, log *slog.Logger) *Pipeline {
	return &Pipeline{
		Cfg: cfg,
		Log: log,
		Run: ExecRunner{},
		HTTP: &http.Client{
			// No overall timeout: the extract may be gigabytes. Stalls are
			// caught by the idle watchdog in Build.fetch.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		},
		Now:  time.Now,
		Poll: 10 * time.Second,
	}
}

// Steps lists the pipeline steps for a build, in execution order.
func (p *Pipeline) Steps(b *Build) []Step {
	return []Step{
		{"prepare", b.Prepare},
		{"download", b.Download},
		{"extract", b.Extract},
		{"osrm", b.OSRM},
		{"tiles", b.Tiles},
		{"nominatim", b.Nominatim},
		{"photon", b.Photon},
		{"smoke", b.Smoke},
		{"release", b.Release},
		{"cleanup", b.Cleanup},
	}
}

// Update runs the full pipeline and returns the new version name. On any
// failure the live version keeps serving and everything the failed build
// created is removed.
func (p *Pipeline) Update(ctx context.Context, opts Options) (string, error) {
	if err := os.MkdirAll(p.Cfg.DataDir, 0o755); err != nil {
		return "", fmt.Errorf("data dir: %w", err)
	}
	unlock, err := lockDir(p.Cfg.DataDir)
	if err != nil {
		return "", err
	}
	defer unlock()

	version := dataset.NewVersion(p.Now())
	if opts.Version != "" {
		if !dataset.ValidVersion(opts.Version) {
			return "", fmt.Errorf("invalid -version %q", opts.Version)
		}
		if _, err := os.Stat(dataset.Dir(p.Cfg.DataDir, opts.Version)); err != nil {
			return "", fmt.Errorf("-version %s: %w", opts.Version, err)
		}
		version = opts.Version
	}
	b := NewBuild(p.Cfg, version, p.Log.With("version", version), p.Run, p.HTTP)
	b.PollInterval = p.Poll
	if p.Probes != nil {
		b.Probes = *p.Probes
	}
	log := b.Log
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return "", err
	}

	steps := p.Steps(b)
	if len(opts.Only) > 0 {
		steps, err = selectSteps(steps, opts.Only)
		if err != nil {
			return "", err
		}
	}
	live, _ := dataset.Current(p.Cfg.DataDir)
	log.Info("update started", "city", p.Cfg.Name, "live_version", live, "steps", stepNames(steps))
	start := p.Now()
	for _, s := range steps {
		t0 := p.Now()
		log.Info("step started", "step", s.Name)
		if err := s.Run(ctx); err != nil {
			log.Error("step failed", "step", s.Name, "err", err)
			if opts.KeepOnFailure || len(opts.Only) > 0 {
				log.Warn("keeping build directory and candidates for inspection", "dir", b.Dir)
			} else {
				// ctx may be cancelled (SIGTERM); cleanup must still run.
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
				b.Discard(cctx)
				cancel()
			}
			return "", fmt.Errorf("step %q failed: %w", s.Name, err)
		}
		d := p.Now().Sub(t0)
		b.record(s.Name, d)
		log.Info("step finished", "step", s.Name, "duration", formatDuration(d))
	}
	log.Info("update finished", "total", formatDuration(p.Now().Sub(start)), "live_version", func() string {
		v, _ := dataset.Current(p.Cfg.DataDir)
		return v
	}())
	return version, nil
}

func selectSteps(all []Step, only []string) ([]Step, error) {
	want := map[string]bool{}
	for _, n := range only {
		want[strings.TrimSpace(n)] = true
	}
	var out []Step
	for _, s := range all {
		if want[s.Name] {
			out = append(out, s)
			delete(want, s.Name)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for n := range want {
			unknown = append(unknown, n)
		}
		return nil, fmt.Errorf("unknown step(s) %v; valid: %v", unknown, stepNames(all))
	}
	return out, nil
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// lockDir takes an exclusive, non-blocking lock so that the scheduled and a
// manual update never run at the same time. The lock is released when the
// process exits, even after a crash.
func lockDir(dataDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dataDir, ".update.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

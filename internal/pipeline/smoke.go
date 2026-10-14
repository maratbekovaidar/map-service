package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/geo"
	"map-service/internal/tiles"
)

// The smoke tests use the gateway's own clients, so they also prove that the
// new data version still speaks the formats the gateway parses.

// NominatimProbe is what the smoke test needs from a Nominatim client.
type NominatimProbe interface {
	Search(ctx context.Context, p nominatim.SearchParams) ([]nominatim.Place, error)
	Reverse(ctx context.Context, lat, lon float64, languages []string) (*nominatim.Place, error)
	Health(ctx context.Context) error
}

// PhotonProbe is what the smoke test needs from a Photon client.
type PhotonProbe interface {
	Search(ctx context.Context, p photon.SearchParams) ([]photon.Feature, error)
	Health(ctx context.Context) error
}

// OSRMProbe is what the smoke test needs from an OSRM client.
type OSRMProbe interface {
	Route(ctx context.Context, p osrm.RouteParams) (*osrm.RouteResponse, error)
	Health(ctx context.Context, pt geo.Point) error
}

// TileProbe reads tiles of a version's archive.
type TileProbe interface {
	Tile(ctx context.Context, version string, z, x, y int) tiles.Result
	Ready(ctx context.Context, version string) error
}

// Probes creates the clients for the candidate services. Production uses the
// real clients; tests substitute fakes.
type Probes struct {
	Nominatim func(baseURL string) NominatimProbe
	Photon    func(baseURL string) PhotonProbe
	OSRM      func(baseURL string) OSRMProbe
	Tiles     func() (TileProbe, error)
}

// sharedTiles is created once per process: go-pmtiles' server owns a
// goroutine that cannot be stopped, and `updater cron` runs for months.
var sharedTiles struct {
	once sync.Once
	srv  *tiles.Server
	err  error
}

// DefaultProbes returns the real clients (10 s timeout per call).
func DefaultProbes(log *slog.Logger, dataDir string) Probes {
	return Probes{
		Nominatim: func(u string) NominatimProbe { return nominatim.New(u, probeTimeout) },
		Photon:    func(u string) PhotonProbe { return photon.New(u, probeTimeout) },
		OSRM:      func(u string) OSRMProbe { return osrm.New(u, probeTimeout) },
		Tiles: func() (TileProbe, error) {
			sharedTiles.once.Do(func() { sharedTiles.srv, sharedTiles.err = tiles.NewServer(dataDir, 8, log) })
			return sharedTiles.srv, sharedTiles.err
		},
	}
}

const (
	probeTimeout = 10 * time.Second
	// defaultCandidateTimeout is how long a candidate container may take to come up.
	defaultCandidateTimeout = 3 * time.Minute
)

func (b *Build) osrmCandidate(profile string) string { return b.Cfg.Project + "-cand-osrm-" + profile }
func (b *Build) photonCandidate() string             { return b.Cfg.Project + "-cand-photon" }

// startCandidate runs a tool service detached under a fixed container name so
// it is reachable on the internal network by that name.
func (b *Build) startCandidate(ctx context.Context, name, service string, env map[string]string, args ...string) error {
	a := append([]string{"run", "-d", "--no-deps", "--name", name, service}, args...)
	if err := b.compose(ctx, env, a...); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

func (b *Build) removeContainer(ctx context.Context, name string) {
	if _, err := b.Run.Output(ctx, Cmd{Name: b.Cfg.DockerBin, Args: []string{"rm", "-f", "-v", name}}); err != nil {
		b.Log.Warn("cannot remove container", "container", name, "err", err)
	}
}

// waitUntil polls fn until it succeeds, the timeout passes or ctx ends.
func (b *Build) waitUntil(ctx context.Context, what string, timeout time.Duration, fn func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		last = fn(cctx)
		cancel()
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become ready within %s: %w", what, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.PollInterval):
		}
	}
}

// Smoke verifies the freshly built version before anything is switched:
// tile read, geocode, reverse, a route per profile and a Photon suggestion.
// The candidate OSRM/Photon helper containers it starts are always removed.
func (b *Build) Smoke(ctx context.Context) error {
	cfg := b.Cfg
	bbox := cfg.BBox
	center := cfg.Center

	var helpers []string
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, h := range helpers {
			b.removeContainer(cctx, h)
		}
	}()

	// Start the helper containers first so they boot while the cheap checks run.
	osrmURL := map[string]string{}
	for _, p := range cfg.Profiles {
		name := b.osrmCandidate(p)
		helpers = append(helpers, name)
		if err := b.startCandidate(ctx, name, "osrm-tools", map[string]string{"OSRM_PROFILE": p},
			"osrm-routed", "--algorithm", "mld", "--port", "5000", "/data/city.osrm"); err != nil {
			return fmt.Errorf("smoke: %w", err)
		}
		osrmURL[p] = "http://" + name + ":5000"
	}
	photonName := b.photonCandidate()
	helpers = append(helpers, photonName)
	if err := b.startCandidate(ctx, photonName, "photon-tools", nil,
		"serve", "-data-dir", "/photon", "-listen-ip", "0.0.0.0", "-listen-port", "2322"); err != nil {
		return fmt.Errorf("smoke: %w", err)
	}

	// 1. Tiles: the archive opens and the city centre tile holds data.
	tp, err := b.Probes.Tiles()
	if err != nil {
		return fmt.Errorf("smoke: tiles: %w", err)
	}
	if err := tp.Ready(ctx, b.Version); err != nil {
		return fmt.Errorf("smoke: tiles: %w", err)
	}
	x, y := geo.TileXY(center, tiles.MaxZoom)
	if r := tp.Tile(ctx, b.Version, tiles.MaxZoom, x, y); r.Status != 200 || len(r.Body) == 0 {
		return fmt.Errorf("smoke: tiles: z%d/%d/%d at the city centre returned status %d with %d bytes", tiles.MaxZoom, x, y, r.Status, len(r.Body))
	}
	b.Log.Info("smoke: tiles ok", "tile", fmt.Sprintf("%d/%d/%d", tiles.MaxZoom, x, y))

	// 2. Nominatim (the candidate that just finished the import).
	nom := b.Probes.Nominatim("http://" + b.candidateContainer() + ":8080")
	if err := b.waitUntil(ctx, "nominatim", b.CandidateTimeout, nom.Health); err != nil {
		return fmt.Errorf("smoke: %w", err)
	}
	found, err := nom.Search(ctx, nominatim.SearchParams{Query: cfg.SmokeGeocodeQuery, Limit: 5, Languages: cfg.Languages, Bounds: &bbox})
	if err != nil {
		return fmt.Errorf("smoke: geocode %q: %w", cfg.SmokeGeocodeQuery, err)
	}
	inside := 0
	for _, p := range found {
		if bbox.Contains(geo.Point{Lon: p.Lon, Lat: p.Lat}) {
			inside++
		}
	}
	if inside < cfg.SmokeGeocodeMinResults {
		return fmt.Errorf("smoke: geocode %q returned %d results inside the city, want at least %d", cfg.SmokeGeocodeQuery, inside, cfg.SmokeGeocodeMinResults)
	}
	rev, err := nom.Reverse(ctx, center.Lat, center.Lon, cfg.Languages)
	if err != nil {
		return fmt.Errorf("smoke: reverse at the city centre (%.5f,%.5f): %w", center.Lat, center.Lon, err)
	}
	if rev.DisplayName == "" {
		return errors.New("smoke: reverse at the city centre returned an empty address")
	}
	b.Log.Info("smoke: nominatim ok", "geocode_results", inside, "reverse", rev.DisplayName)

	// 3. OSRM, every profile.
	for _, p := range cfg.Profiles {
		client := b.Probes.OSRM(osrmURL[p])
		if err := b.waitUntil(ctx, "osrm "+p, b.CandidateTimeout, func(c context.Context) error { return client.Health(c, center) }); err != nil {
			return fmt.Errorf("smoke: %w", err)
		}
		res, err := client.Route(ctx, osrm.RouteParams{Profile: p, Points: []geo.Point{center, nearbyPoint(center, bbox)}})
		if err != nil {
			return fmt.Errorf("smoke: route (%s): %w", p, err)
		}
		r := res.Routes[0]
		if r.Distance <= 0 || r.Duration <= 0 || r.Geometry.Encoded == "" {
			return fmt.Errorf("smoke: route (%s) is degenerate: distance=%v duration=%v", p, r.Distance, r.Duration)
		}
		b.Log.Info("smoke: route ok", "profile", p, "distance_m", int(r.Distance), "duration_s", int(r.Duration))
	}

	// 4. Photon.
	ph := b.Probes.Photon("http://" + photonName + ":2322")
	if err := b.waitUntil(ctx, "photon", b.CandidateTimeout, ph.Health); err != nil {
		return fmt.Errorf("smoke: %w", err)
	}
	feats, err := ph.Search(ctx, photon.SearchParams{Query: cfg.SmokeGeocodeQuery, Limit: 5, Lang: cfg.Languages[0], BBox: &bbox})
	if err != nil {
		return fmt.Errorf("smoke: autocomplete %q: %w", cfg.SmokeGeocodeQuery, err)
	}
	if len(feats) == 0 {
		return fmt.Errorf("smoke: autocomplete %q returned nothing", cfg.SmokeGeocodeQuery)
	}
	b.Log.Info("smoke: photon ok", "results", len(feats), "first", strings.TrimSpace(feats[0].Name))
	return nil
}

// nearbyPoint returns a point about 1 km north-east of c, kept inside bbox.
func nearbyPoint(c geo.Point, bbox geo.BBox) geo.Point {
	p := geo.Point{Lon: c.Lon + 0.013, Lat: c.Lat + 0.009}
	if !bbox.Contains(p) {
		p = geo.Point{Lon: c.Lon - 0.013, Lat: c.Lat - 0.009}
	}
	return p
}

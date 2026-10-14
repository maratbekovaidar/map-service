package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"map-service/internal/dataset"
)

// switchServices lists, in restart order, the services whose data comes from
// the version directory. The gateway is not among them: it re-reads the
// "current" link itself (tiles immediately, cache prefix within seconds).
func (b *Build) switchServices() []string {
	var svcs []string
	for _, p := range b.Cfg.Profiles {
		svcs = append(svcs, "osrm-"+p) // fastest to come back first
	}
	return append(svcs, "photon", "nominatim")
}

// liveVersion is the version data/current points at ("" if none).
func (b *Build) liveVersion() string {
	v, _ := dataset.Current(b.Cfg.DataDir)
	return v
}

// serviceContainer returns the id of the service's container, or "" if the
// stack has no such container (not deployed yet).
func (b *Build) serviceContainer(ctx context.Context, svc string) (string, error) {
	out, err := b.Run.Output(ctx, b.composeCmd(nil, "ps", "-a", "-q", svc))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]), nil
}

// Release makes the verified version live:
//
//  1. write the manifest (marks the version complete),
//  2. stop the candidate Nominatim (its volume becomes the live one),
//  3. atomically repoint data/current,
//  4. recreate the affected services one at a time, waiting for each to be
//     healthy, then wait for the gateway to report the new data version.
//
// If step 4 fails, the previous version is restored.
func (b *Build) Release(ctx context.Context) error {
	cfg := b.Cfg
	prev, _ := dataset.Current(cfg.DataDir)

	m := dataset.Manifest{
		Version: b.Version, CreatedAt: time.Now().UTC(), City: cfg.Name, BBox: cfg.BBox.String(),
		ExtractMode: cfg.ExtractMode, Source: b.source, Profiles: cfg.Profiles, Languages: cfg.Languages, Steps: b.steps,
	}
	if err := dataset.WriteManifest(cfg.DataDir, m); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	// Stop (not kill) the candidate so PostgreSQL shuts down cleanly, then
	// drop the container; the named volume stays and is reused by `nominatim`.
	if _, err := b.Run.Output(ctx, Cmd{Name: cfg.DockerBin, Args: []string{"stop", "--time", "90", b.candidateContainer()}}); err != nil {
		return fmt.Errorf("stop candidate nominatim: %w", err)
	}
	b.removeContainer(ctx, b.candidateContainer())

	if err := dataset.SwitchCurrent(cfg.DataDir, b.Version); err != nil {
		return fmt.Errorf("switch current: %w", err)
	}
	b.Log.Info("data/current switched", "from", prev, "to", b.Version)

	deployed, err := b.deployed(ctx)
	if err != nil {
		return b.rollback(ctx, prev, fmt.Errorf("inspect stack: %w", err))
	}
	if len(deployed.services) == 0 {
		b.Log.Info("the stack is not running; start it with `make up` (it will use the new version)")
		return nil
	}

	swCtx, cancel := context.WithTimeout(ctx, cfg.SwitchTimeout*time.Duration(len(deployed.services)+1))
	defer cancel()
	if err := b.restartServices(swCtx, deployed.services); err != nil {
		return b.rollback(ctx, prev, err)
	}
	if deployed.gateway {
		if err := b.waitGateway(swCtx, b.Version); err != nil {
			return b.rollback(ctx, prev, err)
		}
	}
	return nil
}

type deployment struct {
	services []string // switchServices that have a container
	gateway  bool
}

func (b *Build) deployed(ctx context.Context) (deployment, error) {
	var d deployment
	for _, svc := range b.switchServices() {
		id, err := b.serviceContainer(ctx, svc)
		if err != nil {
			return d, err
		}
		if id != "" {
			d.services = append(d.services, svc)
		}
	}
	id, err := b.serviceContainer(ctx, "gateway")
	if err != nil {
		return d, err
	}
	d.gateway = id != ""
	return d, nil
}

// restartServices recreates the services one by one. DATA_VERSION for
// docker compose comes from data/current, which has just been switched.
func (b *Build) restartServices(ctx context.Context, services []string) error {
	for _, svc := range services {
		b.Log.Info("restarting service", "service", svc, "data_version", b.liveVersion())
		if err := b.compose(ctx, nil, "up", "-d", "--no-deps", "--force-recreate", svc); err != nil {
			return fmt.Errorf("recreate %s: %w", svc, err)
		}
		id, err := b.serviceContainer(ctx, svc)
		if err != nil {
			return fmt.Errorf("find container of %s: %w", svc, err)
		}
		if id == "" {
			return fmt.Errorf("service %s has no container after being recreated", svc)
		}
		if err := b.waitHealthy(ctx, id, b.Cfg.SwitchTimeout); err != nil {
			return fmt.Errorf("%s did not become healthy: %w", svc, err)
		}
	}
	return nil
}

// waitGateway waits until the gateway is ready and serves version.
func (b *Build) waitGateway(ctx context.Context, version string) error {
	hc := &http.Client{Timeout: 5 * time.Second}
	return b.waitUntil(ctx, "gateway readiness on "+version, b.Cfg.SwitchTimeout, func(c context.Context) error {
		req, err := http.NewRequestWithContext(c, http.MethodGet, b.Cfg.GatewayURL+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var rep struct {
			Status      string `json:"status"`
			DataVersion string `json:"data_version"`
		}
		_ = json.Unmarshal(body, &rep)
		if resp.StatusCode != http.StatusOK || rep.Status != "ok" {
			return fmt.Errorf("gateway /readyz: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if rep.DataVersion != version {
			return fmt.Errorf("gateway still serves data version %q", rep.DataVersion)
		}
		return nil
	})
}

// rollback restores the previous version after a failed switch and returns
// cause wrapped with the outcome.
func (b *Build) rollback(ctx context.Context, prev string, cause error) error {
	b.Log.Error("switch failed, rolling back", "to", prev, "err", cause)
	if prev == "" {
		return fmt.Errorf("%w (no previous version to restore)", cause)
	}
	// ctx may be the cancelled switch deadline; recovery needs its own budget.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.Cfg.SwitchTimeout*3)
	defer cancel()
	if err := dataset.SwitchCurrent(b.Cfg.DataDir, prev); err != nil {
		return fmt.Errorf("%w; ROLLBACK FAILED, cannot repoint data/current to %s: %w", cause, prev, err)
	}
	deployed, err := b.deployed(rctx)
	if err == nil {
		err = b.restartServices(rctx, deployed.services)
	}
	if err != nil {
		return fmt.Errorf("%w; data/current restored to %s but restarting services failed: %w", cause, prev, err)
	}
	return fmt.Errorf("%w (rolled back to %s)", cause, prev)
}

// ------------------------------------------------------------------ cleanup

// Cleanup removes old versions (and their Nominatim volumes), keeping the
// live one plus the newest others up to DATA_KEEP_VERSIONS in total, and any
// leftovers of crashed builds. It is best effort: the switch already
// succeeded, so problems here are logged, never fatal.
func (b *Build) Cleanup(ctx context.Context) error {
	versions, err := dataset.Versions(b.Cfg.DataDir)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	cur, _ := dataset.Current(b.Cfg.DataDir)
	keep := map[string]bool{}
	if cur != "" {
		keep[cur] = true
	}
	for i := len(versions) - 1; i >= 0 && len(keep) < b.Cfg.KeepVersions; i-- {
		if dataset.Complete(b.Cfg.DataDir, versions[i]) {
			keep[versions[i]] = true
		}
	}
	for _, v := range versions {
		if keep[v] {
			continue
		}
		b.Log.Info("removing old data version", "version", v, "complete", dataset.Complete(b.Cfg.DataDir, v))
		b.removeVersion(ctx, v)
	}
	return nil
}

func (b *Build) removeVersion(ctx context.Context, v string) {
	vol := b.Cfg.Project + "-nominatim-pg-" + v
	if _, err := b.Run.Output(ctx, Cmd{Name: b.Cfg.DockerBin, Args: []string{"volume", "rm", "-f", vol}}); err != nil {
		b.Log.Warn("cannot remove volume", "volume", vol, "err", err)
	}
	if err := os.RemoveAll(dataset.Dir(b.Cfg.DataDir, v)); err != nil {
		b.Log.Warn("cannot remove version directory", "version", v, "err", err)
	}
}

package httpapi

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"map-service/internal/clients/httpx"
	"map-service/internal/dataset"
)

// GET /healthz - liveness: the process is up.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

const checkTimeout = 2 * time.Second

// readyCache remembers the last readiness report for a second so a load
// balancer polling /readyz does not multiply load on the backends.
type readyCache struct {
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	at   time.Time
	last Readiness
	code int
}

// GET /readyz - every required backend must answer.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) error {
	rep, code := s.readiness(r.Context())
	writeJSON(w, code, rep)
	return nil
}

func (s *Server) readiness(ctx context.Context) (Readiness, int) {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if !s.ready.at.IsZero() && s.ready.now().Sub(s.ready.at) < s.ready.ttl {
		return s.ready.last, s.ready.code
	}
	rep := Readiness{Status: "ok", DataVersion: s.version(), Checks: map[string]CheckResult{}}

	type check struct {
		name     string
		required bool
		fn       func(context.Context) error
	}
	checks := []check{
		{"photon", true, s.photon.Health},
		{"nominatim", true, s.nominatim.Health},
		{"tiles", true, func(c context.Context) error { return s.tiles.Ready(c, dataset.CurrentLink) }},
	}
	profiles := make([]string, 0, len(s.osrm))
	for p := range s.osrm {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	for _, p := range profiles {
		client := s.osrm[p]
		checks = append(checks, check{"osrm-" + p, true, func(c context.Context) error { return client.Health(c, s.cfg.Center) }})
	}
	if s.cache.Enabled() {
		checks = append(checks, check{"redis", false, s.cache.Ping})
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			start := s.now()
			err := c.fn(cctx)
			res := CheckResult{Status: "ok", LatencyMS: s.now().Sub(start).Milliseconds()}
			if err != nil {
				res.Status = "down"
				res.Error = "unavailable"
				if httpx.IsTimeout(err) {
					res.Error = "timeout"
				}
				// Details stay in the log: /readyz is public and must not leak internals.
				loggerFrom(ctx).Warn("readiness check failed", "check", c.name, "err", err)
			}
			mu.Lock()
			rep.Checks[c.name] = res
			if res.Status != "ok" && c.required {
				rep.Status = "unavailable"
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !s.cache.Enabled() {
		rep.Checks["redis"] = CheckResult{Status: "disabled"}
	}

	code := http.StatusOK
	if rep.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	s.ready.at, s.ready.last, s.ready.code = s.ready.now(), rep, code
	return rep, code
}

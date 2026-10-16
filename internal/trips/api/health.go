package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"map-service/internal/httpkit"
)

// GET /healthz - liveness: the process is up.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) error {
	httpkit.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

// Readiness is the answer to GET /readyz.
type Readiness struct {
	Status string            `json:"status"` // ok | unavailable
	Checks map[string]string `json:"checks"` // name -> ok | down
}

// GET /readyz - the database and the car router must answer.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) error {
	checks := map[string]func(context.Context) error{
		"db":       s.store.Ping,
		"osrm-car": func(ctx context.Context) error { return s.router.Health(ctx, s.cfg.Center) },
	}
	rep := Readiness{Status: "ok", Checks: map[string]string{}}
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for name, check := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			err := check(ctx)
			mu.Lock()
			defer mu.Unlock()
			rep.Checks[name] = "ok"
			if err != nil {
				// Details stay in the log: /readyz is public.
				httpkit.LoggerFrom(r.Context()).Warn("readiness check failed", "check", name, "err", err)
				rep.Checks[name], rep.Status = "down", "unavailable"
			}
		})
	}
	wg.Wait()
	code := http.StatusOK
	if rep.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	httpkit.WriteJSON(w, code, rep)
	return nil
}

// Sweep expires the active trips that have been silent for IdleTimeout and
// deletes the trips that ended more than Retention ago.
func (s *Server) Sweep(ctx context.Context) {
	now := s.now()
	expired, err := s.store.ExpireIdle(ctx, now.Add(-s.cfg.IdleTimeout))
	if err != nil {
		s.log.Error("expiring idle trips failed", "err", err)
	} else if len(expired) > 0 {
		s.log.Info("idle trips expired", "count", len(expired))
	}
	purged, err := s.store.Purge(ctx, now.Add(-s.cfg.Retention))
	if err != nil {
		s.log.Error("purging old trips failed", "err", err)
	} else if purged > 0 {
		s.log.Info("old trips deleted", "count", purged, "retention", s.cfg.Retention.String())
	}
}

// RunSweeper calls Sweep every interval until ctx is done. Several instances
// may run it at once: expiry skips trips that are locked, deletes are
// idempotent.
func (s *Server) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

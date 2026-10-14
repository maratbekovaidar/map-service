package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// NextRun returns the first time after t at which spec fires. spec is the
// usual 5-field cron expression (or @weekly, @daily, ...), interpreted in
// UTC unless it starts with CRON_TZ=<zone>.
func NextRun(spec string, t time.Time) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if !strings.HasPrefix(spec, "CRON_TZ=") && !strings.HasPrefix(spec, "TZ=") {
		spec = "CRON_TZ=UTC " + spec
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid UPDATE_CRON %q: %w", spec, err)
	}
	return sched.Next(t), nil
}

// RunCron runs an update at every firing of cfg.Cron until ctx ends. A failed
// update is logged and retried at the next firing; the loop never exits
// because of it.
func (p *Pipeline) RunCron(ctx context.Context) error {
	if _, err := NextRun(p.Cfg.Cron, p.Now()); err != nil {
		return err
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	for {
		next, _ := NextRun(p.Cfg.Cron, p.Now())
		p.Log.Info("next scheduled update", "at", next.UTC().Format(time.RFC3339), "cron", p.Cfg.Cron)
		if err := sleep(ctx, next.Sub(p.Now())); err != nil {
			return nil // shutting down
		}
		version, err := p.Update(ctx, Options{})
		switch {
		case errors.Is(err, ErrAlreadyRunning):
			p.Log.Info("scheduled update skipped: another update is running")
		case err != nil:
			p.Log.Error("scheduled update failed; the live version is unchanged", "err", err)
		default:
			p.Log.Info("scheduled update finished", "version", version)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d < 0 {
		d = 0
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

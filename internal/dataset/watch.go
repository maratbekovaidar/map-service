package dataset

import (
	"context"
	"sync/atomic"
	"time"
)

// NoVersion is reported while no version has been switched live.
const NoVersion = "none"

// Watcher tracks the live data version by polling the "current" symlink, so
// the gateway notices a switch without a restart (cache key prefix, /readyz).
type Watcher struct {
	dir      string
	interval time.Duration
	v        atomic.Value // string
}

// NewWatcher reads the version once, synchronously.
func NewWatcher(dataDir string, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	w := &Watcher{dir: dataDir, interval: interval}
	w.refresh()
	return w
}

func (w *Watcher) refresh() {
	v, err := Current(w.dir)
	if err != nil {
		v = NoVersion
	}
	w.v.Store(v)
}

// Version returns the live version, or NoVersion.
func (w *Watcher) Version() string { return w.v.Load().(string) }

// Run polls until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.refresh()
		}
	}
}

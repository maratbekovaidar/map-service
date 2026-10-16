package httpkit

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"map-service/internal/config"
)

// Limiter keeps one token bucket per key (API key id, token or client IP).
type Limiter struct {
	limit rate.Limit
	burst int
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

const (
	idleBucketTTL = 10 * time.Minute
	sweepEvery    = time.Minute
)

// NewLimiter returns nil when the limit is disabled (RPS == 0); a nil
// *Limiter allows everything.
func NewLimiter(l config.Limit, now func() time.Time) *Limiter {
	if l.RPS <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{limit: rate.Limit(l.RPS), burst: max(l.Burst, 1), now: now, buckets: map[string]*bucket{}}
}

// Allow consumes one token of key's bucket. When the bucket is empty it
// returns false and how long the caller should wait.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b := l.buckets[key]
	if b == nil {
		b = &bucket{lim: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	r := b.lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now) // do not consume a token for a rejected request
		return false, d
	}
	return true, 0
}

// sweep drops buckets idle for idleBucketTTL (called with l.mu held).
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idleBucketTTL {
			delete(l.buckets, k)
		}
	}
}

// Len reports the number of live buckets (tests, diagnostics).
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// LimitByIP rejects clients that exceed their per-IP bucket.
func LimitByIP(l *Limiter, ip func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, wait := l.Allow("ip:" + ip(r)); !ok {
				LoggerFrom(r.Context()).Info("rate limited", "by", "ip")
				WriteError(w, RateLimited(wait))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// LimitByKey rejects API keys that exceed their bucket. It must run after
// authentication, which stores the key id in the request context.
func LimitByKey(l *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := KeyIDFrom(r.Context())
			if id == "" {
				next.ServeHTTP(w, r)
				return
			}
			if ok, wait := l.Allow("key:" + id); !ok {
				LoggerFrom(r.Context()).Info("rate limited", "by", "key")
				WriteError(w, RateLimited(wait))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

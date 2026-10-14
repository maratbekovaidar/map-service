// Package cache wraps Redis for response caching. The cache is strictly
// optional: every failure degrades to a cache miss and is logged as a
// warning, so the gateway keeps working when Redis is down.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"map-service/internal/geo"
)

// Store is what the handlers need from a cache.
type Store interface {
	// GetJSON loads key into v and reports whether it was a hit.
	GetJSON(ctx context.Context, key string, v any) bool
	// SetJSON stores v under key for ttl.
	SetJSON(ctx context.Context, key string, v any, ttl time.Duration)
	// Ping checks the backend (used by /readyz).
	Ping(ctx context.Context) error
	// Enabled reports whether a backend is configured at all.
	Enabled() bool
}

// Noop is a Store that never caches (REDIS_ADDR empty, or tests).
type Noop struct{}

func (Noop) GetJSON(context.Context, string, any) bool           { return false }
func (Noop) SetJSON(context.Context, string, any, time.Duration) {}
func (Noop) Ping(context.Context) error                          { return nil }
func (Noop) Enabled() bool                                       { return false }

// Options configure the Redis store.
type Options struct {
	Addr     string
	Password string
	DB       int
	// Backoff is how long Redis is skipped after a failure, so that a dead
	// Redis costs one failed attempt per Backoff instead of one per request.
	Backoff time.Duration
}

// Redis is a Store backed by Redis.
type Redis struct {
	rdb     *redis.Client
	log     *slog.Logger
	backoff time.Duration

	downUntil atomic.Int64 // unix nanos; Redis is skipped until then
	warnMu    sync.Mutex
	lastWarn  time.Time
	now       func() time.Time
}

// redisLogger routes go-redis' internal messages (pool dial failures, ...)
// into slog at debug level. Unrouted, the library prints them to stderr as
// unstructured text on every failed dial while Redis is down; the cache
// already reports an outage once per backoff window.
type redisLogger struct{ log *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.log.DebugContext(ctx, fmt.Sprintf(format, v...), "component", "go-redis")
}

// NewRedis connects lazily; it never fails because Redis is unreachable.
func NewRedis(opts Options, log *slog.Logger) *Redis {
	redis.SetLogger(redisLogger{log})
	if opts.Backoff <= 0 {
		opts.Backoff = 5 * time.Second
	}
	return &Redis{
		rdb: redis.NewClient(&redis.Options{
			Addr:            opts.Addr,
			Password:        opts.Password,
			DB:              opts.DB,
			DialTimeout:     300 * time.Millisecond,
			ReadTimeout:     200 * time.Millisecond,
			WriteTimeout:    200 * time.Millisecond,
			PoolSize:        32,
			MaxRetries:      -1, // no retries: a miss is cheaper than latency
			DisableIdentity: true,
		}),
		log:     log,
		backoff: opts.Backoff,
		now:     time.Now,
	}
}

// Close releases the connection pool.
func (r *Redis) Close() error { return r.rdb.Close() }

// Enabled implements Store.
func (r *Redis) Enabled() bool { return true }

func (r *Redis) skip() bool { return r.now().UnixNano() < r.downUntil.Load() }

func (r *Redis) fail(op string, err error) {
	r.downUntil.Store(r.now().Add(r.backoff).UnixNano())
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.now().Sub(r.lastWarn) >= r.backoff {
		r.lastWarn = r.now()
		r.log.Warn("redis unavailable, serving without cache", "op", op, "err", err, "retry_in", r.backoff.String())
	}
}

// GetJSON implements Store.
func (r *Redis) GetJSON(ctx context.Context, key string, v any) bool {
	if r.skip() {
		return false
	}
	b, err := r.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false
	}
	if err != nil {
		r.fail("get", err)
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		// A corrupt entry is treated as a miss; it is overwritten on Set.
		r.log.Warn("cache entry is not valid JSON, ignoring", "key", key, "err", err)
		return false
	}
	return true
}

// SetJSON implements Store.
func (r *Redis) SetJSON(ctx context.Context, key string, v any, ttl time.Duration) {
	if r.skip() || ttl <= 0 {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		r.log.Warn("cache: cannot marshal value", "key", key, "err", err)
		return
	}
	if err := r.rdb.Set(ctx, key, b, ttl).Err(); err != nil {
		r.fail("set", err)
	}
}

// Ping implements Store. It ignores the failure backoff on purpose so
// /readyz reports the real state.
func (r *Redis) Ping(ctx context.Context) error { return r.rdb.Ping(ctx).Err() }

// ------------------------------------------------------------ key building

// KeyPrefix is the namespace of all keys. The data version is part of it, so
// after a data rebuild the old entries are simply never read again (and age
// out through their TTL / LRU eviction).
func KeyPrefix(dataVersion string) string { return "map:v1:" + dataVersion + ":" }

// NormalizeQuery lower-cases, trims and collapses whitespace so that
// "  Байтерек " and "байтерек" share a cache entry.
func NormalizeQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func hashOf(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

// ReverseKey: coordinates rounded to 5 decimals (~1 m) plus language.
func ReverseKey(dataVersion string, lat, lon float64, lang string) string {
	return fmt.Sprintf("%sreverse:%s,%s:%s", KeyPrefix(dataVersion), geo.FormatCoord(lat), geo.FormatCoord(lon), lang)
}

// GeocodeKey: normalized query + limit + language.
func GeocodeKey(dataVersion, q string, limit int, lang string) string {
	return KeyPrefix(dataVersion) + "geocode:" + lang + ":" + hashOf(NormalizeQuery(q), fmt.Sprint(limit))
}

// AutocompleteKey: normalized query + language + limit, plus the location bias
// rounded to 2 decimals (~1 km) when present: users in different parts of the
// city get differently ranked suggestions, while one user's keystrokes (and
// neighbours) share entries. Photon's bias is soft, so 1 km is below its
// resolution.
func AutocompleteKey(dataVersion, q string, limit int, lang string, lat, lon *float64) string {
	bias := ""
	if lat != nil && lon != nil {
		bias = fmt.Sprintf("%.2f,%.2f", *lat, *lon)
	}
	return KeyPrefix(dataVersion) + "autocomplete:" + lang + ":" + hashOf(NormalizeQuery(q), fmt.Sprint(limit), bias)
}

// RouteKey: profile + points rounded to 5 decimals + options.
func RouteKey(dataVersion, profile string, points []geo.Point, alternatives, steps bool, geometry string) string {
	var sb strings.Builder
	for i, p := range points {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(geo.FormatCoord(p.Lon))
		sb.WriteByte(',')
		sb.WriteString(geo.FormatCoord(p.Lat))
	}
	return fmt.Sprintf("%sroute:%s:%s:alt=%t:steps=%t:geom=%s", KeyPrefix(dataVersion), profile, sb.String(), alternatives, steps, geometry)
}

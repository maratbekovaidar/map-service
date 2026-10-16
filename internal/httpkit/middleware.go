package httpkit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const (
	ctxLoggerKey ctxKey = iota
	ctxInfoKey
)

// reqInfo is request state that inner middleware fills in for the outer
// access log (which only holds the original *http.Request).
type reqInfo struct{ keyID string }

// LoggerFrom returns the request-scoped logger (it carries the request id).
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxLoggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// KeyIDFrom returns the id of the API key (or token) that authenticated the
// request, as set by SetKeyID.
func KeyIDFrom(ctx context.Context) string {
	if info, ok := ctx.Value(ctxInfoKey).(*reqInfo); ok {
		return info.keyID
	}
	return ""
}

// SetKeyID records who authenticated the request, for the access log and
// per-key rate limits. It only works under Observe.
func SetKeyID(ctx context.Context, id string) {
	if info, ok := ctx.Value(ctxInfoKey).(*reqInfo); ok {
		info.keyID = id
	}
}

// statusWriter records what was written so the access log can report it.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// validRequestID accepts a client-supplied id only if it is short and tame
// (it ends up in logs and response headers).
func validRequestID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// Observe is the outermost middleware: request id, request-scoped logger,
// panic recovery and the access log. Query strings are deliberately not
// logged (they contain user-typed addresses and coordinates).
func Observe(log *slog.Logger, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := now()
			id := r.Header.Get("X-Request-Id")
			if !validRequestID(id) {
				id = newRequestID()
			}
			w.Header().Set("X-Request-Id", id)
			rl := log.With("request_id", id)
			ctx := context.WithValue(r.Context(), ctxLoggerKey, rl)
			ctx = context.WithValue(ctx, ctxInfoKey, &reqInfo{})
			r = r.WithContext(ctx)
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				if rec := recover(); rec != nil {
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec) // net/http's own signal to abort the response silently
					}
					rl.Error("panic in handler", "panic", rec, "stack", string(debug.Stack()))
					if !sw.wrote {
						WriteError(sw, Internal(nil))
					}
				}
				level := slog.LevelInfo
				switch {
				case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
					level = slog.LevelDebug // probes would drown the log
				case sw.status >= 500:
					level = slog.LevelError
				case sw.status >= 400:
					level = slog.LevelWarn
				}
				attrs := []any{
					"method", r.Method, "path", r.URL.Path, "status", sw.status,
					"bytes", sw.bytes, "duration_ms", now().Sub(start).Milliseconds(),
				}
				if k := KeyIDFrom(r.Context()); k != "" {
					attrs = append(attrs, "key_id", k)
				}
				rl.Log(r.Context(), level, "request", attrs...)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// ------------------------------------------------------------------- CORS

// CORS answers preflight requests and adds CORS headers for allowed origins.
// methods and headers are what a preflight may ask for ("GET, POST, OPTIONS").
func CORS(origins []string, methods, headers string) func(http.Handler) http.Handler {
	anyOrigin := false
	allowed := map[string]bool{}
	for _, o := range origins {
		if o == "*" {
			anyOrigin = true
		}
		allowed[strings.TrimRight(o, "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			ok := origin != "" && (anyOrigin || allowed[origin])
			if ok {
				h := w.Header()
				if anyOrigin {
					h.Set("Access-Control-Allow-Origin", "*")
				} else {
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
				}
				h.Set("Access-Control-Expose-Headers", "ETag, X-Request-Id, Retry-After")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Methods", methods)
					h.Set("Access-Control-Allow-Headers", headers)
					h.Set("Access-Control-Max-Age", "86400")
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ------------------------------------------------------------------- auth

// Authenticator checks the X-API-Key header against the configured keys.
type Authenticator struct {
	keys     [][]byte
	disabled bool
}

// NewAuthenticator builds an Authenticator. With disabled=true every request
// passes (explicit opt-out for local development).
func NewAuthenticator(keys []string, disabled bool) *Authenticator {
	a := &Authenticator{disabled: disabled}
	for _, k := range keys {
		if k != "" {
			a.keys = append(a.keys, []byte(k))
		}
	}
	return a
}

// Check returns the key id for a valid key. The comparison visits every
// configured key and is constant-time per key.
func (a *Authenticator) Check(key string) (string, bool) {
	if a.disabled {
		return "anonymous", true
	}
	if key == "" {
		return "", false
	}
	kb := []byte(key)
	found := 0
	for _, k := range a.keys {
		found |= subtle.ConstantTimeCompare(kb, k)
	}
	if found != 1 {
		return "", false
	}
	return keyID(key), true
}

// keyID is a short, non-reversible label for logs and rate-limit buckets.
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// Require rejects requests without a valid key.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.Check(r.Header.Get("X-API-Key"))
		if !ok {
			LoggerFrom(r.Context()).Info("rejected request: bad API key")
			WriteError(w, Unauthorized("missing or invalid API key"))
			return
		}
		SetKeyID(r.Context(), id)
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- client IP

// ClientIP returns the address of the real client. X-Forwarded-For is only
// believed when the connection comes from a trusted proxy; then the list is
// walked from the right and the first address that is not itself a trusted
// proxy wins (so a spoofed left-hand entry is ignored).
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := remoteAddr(r.RemoteAddr)
	if !remote.IsValid() || !isTrusted(remote, trusted) {
		if remote.IsValid() {
			return remote.String()
		}
		return r.RemoteAddr
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			continue
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return remote.String()
}

// FromTrustedProxy reports whether the connection comes from one of trusted
// (so that its X-Forwarded-* headers may be believed).
func FromTrustedProxy(r *http.Request, trusted []netip.Prefix) bool {
	return isTrusted(remoteAddr(r.RemoteAddr), trusted)
}

func remoteAddr(s string) netip.Addr {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Chain applies middlewares so that the first one listed is the outermost.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// HandlerFunc is a handler that returns its error instead of writing it.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Wrap turns an error-returning handler into an http.Handler that renders
// errors in the unified format and logs server-side failures.
func Wrap(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		e := AsError(err)
		if e.Status >= 500 && e.Status != StatusClientClosed {
			LoggerFrom(r.Context()).Error("request failed", "code", e.Code, "err", e.Err, "msg", e.Message)
		}
		WriteError(w, e)
	})
}

// Route registers h for pattern and answers any method other than method
// (HEAD is allowed wherever GET is) with a JSON 405. The method is checked
// here rather than in the mux pattern because ServeMux rejects overlapping
// "GET /tiles/{z}/{x}/{file}" and "/tiles/fonts/{stack}/{file}" style pairs.
func Route(mux *http.ServeMux, method, pattern string, h http.Handler) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		headOnGet := method == http.MethodGet && r.Method == http.MethodHead
		if r.Method != method && !headOnGet {
			WriteError(w, MethodNotAllowed(method))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ProbeHealthz calls /healthz of the server listening on listenAddr (":8080"
// means this host) and fails unless it answers 200: the Docker healthcheck.
func ProbeHealthz(listenAddr string) error {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("status " + resp.Status)
	}
	return nil
}

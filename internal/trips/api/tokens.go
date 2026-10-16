package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"

	"map-service/internal/httpkit"
	"map-service/internal/trips/store"
)

// role is what a trip token lets its holder do.
type role string

const (
	driver  role = "driver"  // reports positions, ends the trip
	watcher role = "watcher" // reads where the car is
)

// Tokens are a role prefix and 32 random bytes in unpadded base64url. Only
// their SHA-256 is stored, so a database leak does not hand out access.
var tokenPrefix = map[role]string{driver: "drv_", watcher: "wch_"}

const tokenBytes = 32

var tokenLen = base64.RawURLEncoding.EncodedLen(tokenBytes)

func newToken(r role) (string, []byte, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := tokenPrefix[r] + base64.RawURLEncoding.EncodeToString(b)
	return tok, hashToken(tok), nil
}

func hashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

// parseBearer extracts a well-formed trip token from an Authorization header
// and tells whose it claims to be.
func parseBearer(header string) (string, role, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", "", false
	}
	tok = strings.TrimSpace(tok)
	for r, prefix := range tokenPrefix {
		rest, found := strings.CutPrefix(tok, prefix)
		if !found || len(rest) != tokenLen {
			continue
		}
		if _, err := base64.RawURLEncoding.DecodeString(rest); err != nil {
			return "", "", false
		}
		return tok, r, true
	}
	return "", "", false
}

func badToken() *httpkit.Error {
	return httpkit.Unauthorized("missing or invalid trip token (Authorization: Bearer drv_... or wch_...)")
}

// caller is the trip a request is about and the role of its token.
type caller struct {
	trip *store.Trip
	role role
}

type tripHandler func(w http.ResponseWriter, r *http.Request, c caller) error

// withTrip authenticates the trip token of /v1/trips/{id}/... requests,
// checks that its role may call the endpoint and applies the per-trip limit.
func (s *Server) withTrip(h tripHandler, roles ...role) httpkit.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		tok, claimed, ok := parseBearer(r.Header.Get("Authorization"))
		if !ok {
			return badToken()
		}
		trip, err := s.store.Trip(r.Context(), r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) {
			return httpkit.NotFound("no such trip")
		}
		if err != nil {
			return httpkit.Internal(err)
		}
		want := trip.DriverTokenHash
		if claimed == watcher {
			want = trip.WatchTokenHash
		}
		if subtle.ConstantTimeCompare(hashToken(tok), want) != 1 {
			httpkit.LoggerFrom(r.Context()).Info("rejected request: wrong trip token")
			return badToken()
		}
		// Short, non-secret label for the access log: the role and the random
		// tail of the trip id (UUIDv7 ids start with a timestamp).
		httpkit.SetKeyID(r.Context(), string(claimed)+":"+trip.ID[len(trip.ID)-8:])
		if !slices.Contains(roles, claimed) {
			return httpkit.Forbidden("the %s token may not do this", claimed)
		}
		if ok, wait := s.tripLim.Allow("trip:" + trip.ID + ":" + string(claimed)); !ok {
			httpkit.LoggerFrom(r.Context()).Info("rate limited", "by", "trip")
			return httpkit.RateLimited(wait)
		}
		return h(w, r, caller{trip: trip, role: claimed})
	}
}

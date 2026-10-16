package httpapi

import (
	"net/http"

	"map-service/internal/httpkit"
)

// The HTTP plumbing the gateway shares with the trips service lives in
// httpkit; these names keep the gateway's handlers reading as before.

// Error is an API error: it knows its HTTP status and public message.
type Error = httpkit.Error

// Limiter keeps one token bucket per key.
type Limiter = httpkit.Limiter

// Authenticator checks the X-API-Key header against the configured keys.
type Authenticator = httpkit.Authenticator

// Error codes of the API (see api/openapi.yaml).
const (
	CodeInvalidArgument     = httpkit.CodeInvalidArgument
	CodeUnauthorized        = httpkit.CodeUnauthorized
	CodeForbidden           = httpkit.CodeForbidden
	CodeNotFound            = httpkit.CodeNotFound
	CodeNoRoute             = httpkit.CodeNoRoute
	CodeMethodNotAllowed    = httpkit.CodeMethodNotAllowed
	CodeRateLimited         = httpkit.CodeRateLimited
	CodeUpstreamUnavailable = httpkit.CodeUpstreamUnavailable
	CodeUpstreamTimeout     = httpkit.CodeUpstreamTimeout
	CodeInternal            = httpkit.CodeInternal
)

var (
	// NewLimiter returns nil (allow everything) when the limit is disabled.
	NewLimiter = httpkit.NewLimiter
	// NewAuthenticator builds an Authenticator.
	NewAuthenticator = httpkit.NewAuthenticator

	badRequest    = httpkit.BadRequest
	forbidden     = httpkit.Forbidden
	notFound      = httpkit.NotFound
	noRoute       = httpkit.NoRoute
	internalError = httpkit.Internal
	upstreamError = httpkit.Upstream
	writeError    = httpkit.WriteError
	writeJSON     = httpkit.WriteJSON
	loggerFrom    = httpkit.LoggerFrom
	setKeyID      = httpkit.SetKeyID
	observe       = httpkit.Observe
	clientIP      = httpkit.ClientIP
	chain         = httpkit.Chain
	route         = httpkit.Route
	limitByIP     = httpkit.LimitByIP
	limitByKey    = httpkit.LimitByKey
)

// cors allows what the gateway's endpoints need from a browser.
func cors(origins []string) func(http.Handler) http.Handler {
	return httpkit.CORS(origins, "GET, HEAD, PUT, OPTIONS", "X-API-Key, Content-Type, If-None-Match, X-Request-Id")
}

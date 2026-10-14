// Package httpapi implements the gateway's HTTP API: handlers, middleware and
// DTOs. It talks to the upstream services only through the small interfaces
// declared in server.go, so every behaviour is unit-testable with fakes.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"map-service/internal/clients/httpx"
)

// Error codes of the API (see api/openapi.yaml).
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeUnauthorized        = "unauthorized"
	CodeNotFound            = "not_found"
	CodeNoRoute             = "no_route"
	CodeMethodNotAllowed    = "method_not_allowed"
	CodeRateLimited         = "rate_limited"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodeUpstreamTimeout     = "upstream_timeout"
	CodeInternal            = "internal"
)

// Error is an API error: it knows its HTTP status and public message.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration // set for 429
	Err        error         // underlying cause, logged but never sent
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func badRequest(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func unauthorized() *Error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: "missing or invalid API key"}
}

func notFound(format string, args ...any) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

func noRoute() *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNoRoute, Message: "no route between the given points"}
}

func methodNotAllowed(allow string) *Error {
	return &Error{Status: http.StatusMethodNotAllowed, Code: CodeMethodNotAllowed, Message: "method not allowed, use " + allow}
}

func rateLimited(retry time.Duration) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: CodeRateLimited, Message: "rate limit exceeded", RetryAfter: retry}
}

func internalError(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "internal error", Err: err}
}

// upstreamError turns a failure of a backend call into 504 (timeout) or 502.
// what names the service for the message ("routing", "geocoding", ...).
func upstreamError(what string, err error) *Error {
	if httpx.IsTimeout(err) {
		return &Error{Status: http.StatusGatewayTimeout, Code: CodeUpstreamTimeout, Message: what + " service timed out", Err: err}
	}
	return &Error{Status: http.StatusBadGateway, Code: CodeUpstreamUnavailable, Message: what + " service is unavailable", Err: err}
}

// statusClientClosed is the de-facto status for a client that went away.
const statusClientClosed = 499

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError renders e in the unified error format.
func writeError(w http.ResponseWriter, e *Error) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if e.RetryAfter > 0 {
		secs := int((e.RetryAfter + time.Second - 1) / time.Second)
		h.Set("Retry-After", strconv.Itoa(max(secs, 1)))
	}
	if e.Status == http.StatusMethodNotAllowed {
		// message carries the allowed method for humans; header for machines
		h.Set("Allow", "GET, HEAD, OPTIONS")
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(marshalJSON(errorBody{Error: errorDetail{Code: e.Code, Message: e.Message}}))
}

// marshalJSON encodes v followed by a newline, WITHOUT Go's default HTML
// escaping: "<", ">" and "&" are legal in JSON and place names like
// "Tom & Jerry" or messages like "0 <= z" should stay readable.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(`{"error":{"code":"internal","message":"internal error"}}` + "\n")
	}
	return buf.Bytes()
}

// writeJSON renders a 2xx JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(marshalJSON(v))
}

// asError converts any error into an *Error. Cancelled requests (the client
// went away) are reported as 499 so they are not counted as server faults.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Status: statusClientClosed, Code: CodeInternal, Message: "request cancelled", Err: err}
	}
	return internalError(err)
}

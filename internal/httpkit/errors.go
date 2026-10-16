// Package httpkit is the HTTP plumbing shared by the gateway and the trips
// service: the unified error format, request ids and the access log, CORS,
// API-key authentication, client IP behind a proxy and rate limiting.
package httpkit

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

// Error codes shared by the APIs (see api/openapi.yaml).
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
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
	Allow      string        // set for 405
	Err        error         // underlying cause, logged but never sent
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// BadRequest is a 400 invalid_argument.
func BadRequest(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

// Unauthorized is a 401 with the given message ("missing or invalid API key").
func Unauthorized(msg string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: msg}
}

// Forbidden is a 403.
func Forbidden(format string, args ...any) *Error {
	return &Error{Status: http.StatusForbidden, Code: CodeForbidden, Message: fmt.Sprintf(format, args...)}
}

// NotFound is a 404 not_found.
func NotFound(format string, args ...any) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

// NoRoute is a 404 no_route: the router found no way between the points.
func NoRoute() *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNoRoute, Message: "no route between the given points"}
}

// MethodNotAllowed is a 405 with the Allow header for method.
func MethodNotAllowed(method string) *Error {
	allow := method + ", OPTIONS"
	if method == http.MethodGet {
		allow = "GET, HEAD, OPTIONS"
	}
	return &Error{Status: http.StatusMethodNotAllowed, Code: CodeMethodNotAllowed, Message: "method not allowed, use " + method, Allow: allow}
}

// RateLimited is a 429 with Retry-After.
func RateLimited(retry time.Duration) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: CodeRateLimited, Message: "rate limit exceeded", RetryAfter: retry}
}

// Internal is a 500; err is logged, never sent.
func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "internal error", Err: err}
}

// Upstream turns a failure of a backend call into 504 (timeout) or 502.
// what names the service for the message ("routing", "geocoding", ...).
func Upstream(what string, err error) *Error {
	if httpx.IsTimeout(err) {
		return &Error{Status: http.StatusGatewayTimeout, Code: CodeUpstreamTimeout, Message: what + " service timed out", Err: err}
	}
	return &Error{Status: http.StatusBadGateway, Code: CodeUpstreamUnavailable, Message: what + " service is unavailable", Err: err}
}

// StatusClientClosed is the de-facto status for a client that went away.
const StatusClientClosed = 499

// ErrorBody is the unified error format: {"error": {"code", "message"}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inside of an ErrorBody.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError renders e in the unified error format.
func WriteError(w http.ResponseWriter, e *Error) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if e.RetryAfter > 0 {
		secs := int((e.RetryAfter + time.Second - 1) / time.Second)
		h.Set("Retry-After", strconv.Itoa(max(secs, 1)))
	}
	if e.Allow != "" {
		// message carries the allowed method for humans; header for machines
		h.Set("Allow", e.Allow)
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(MarshalJSON(ErrorBody{Error: ErrorDetail{Code: e.Code, Message: e.Message}}))
}

// MarshalJSON encodes v followed by a newline, WITHOUT Go's default HTML
// escaping: "<", ">" and "&" are legal in JSON and place names like
// "Tom & Jerry" or messages like "0 <= z" should stay readable.
func MarshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(`{"error":{"code":"internal","message":"internal error"}}` + "\n")
	}
	return buf.Bytes()
}

// WriteJSON renders a 2xx JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(MarshalJSON(v))
}

// AsError converts any error into an *Error. Cancelled requests (the client
// went away) are reported as 499 so they are not counted as server faults.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Status: StatusClientClosed, Code: CodeInternal, Message: "request cancelled", Err: err}
	}
	return Internal(err)
}

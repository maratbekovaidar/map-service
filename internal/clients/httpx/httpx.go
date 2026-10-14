// Package httpx is the small HTTP layer shared by the OSRM, Nominatim and
// Photon clients: per-call timeouts, bounded bodies, typed status errors and a
// single retry for connection-level failures (a backend that is restarting).
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// MaxBody bounds the size of an upstream response we are willing to read.
const MaxBody = 8 << 20

// StatusError is returned for non-2xx answers.
type StatusError struct {
	Status int
	Body   string // truncated
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream returned HTTP %d: %s", e.Status, e.Body)
}

// Client performs GET requests against one upstream.
type Client struct {
	HC      *http.Client
	Timeout time.Duration
	// RetryDelay is the pause before the single retry of a connection-level failure.
	RetryDelay time.Duration
}

// New returns a Client whose calls are bounded by timeout.
func New(timeout time.Duration) *Client {
	return &Client{
		HC: &http.Client{Transport: &http.Transport{
			Proxy:                 nil, // upstreams are internal; never go through an env proxy
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
		}},
		Timeout:    timeout,
		RetryDelay: 150 * time.Millisecond,
	}
}

// Get performs a GET and returns the body of a 2xx answer.
func (c *Client) Get(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	body, err := c.do(ctx, rawURL)
	if err != nil && retryable(err) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(c.RetryDelay):
			body, err = c.do(ctx, rawURL)
		}
	}
	return body, err
}

// GetJSON performs a GET and decodes a 2xx JSON answer into out.
func (c *Client) GetJSON(ctx context.Context, rawURL string, out any) error {
	b, err := c.Get(ctx, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode upstream response: %w", err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "map-service-gateway/1.0")
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", MaxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return b, &StatusError{Status: resp.StatusCode, Body: truncate(string(b), 300)}
	}
	return b, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// retryable reports connection-level failures that happen when a container
// is (re)starting: nothing was processed, so repeating a GET is safe.
func retryable(err error) bool {
	if err == nil || IsTimeout(err) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// IsTimeout reports whether err is a deadline or network timeout.
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

package httpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetJSONOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	var out struct{ A int }
	if err := New(time.Second).GetJSON(context.Background(), srv.URL, &out); err != nil || out.A != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestStatusErrorCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 1000), http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := New(time.Second).Get(context.Background(), srv.URL)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 502 {
		t.Fatalf("want StatusError 502, got %v", err)
	}
	if len(se.Body) > 310 {
		t.Errorf("body must be truncated, len=%d", len(se.Body))
	}
}

func TestInvalidJSONIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer srv.Close()
	var out map[string]any
	if err := New(time.Second).GetJSON(context.Background(), srv.URL, &out); err == nil {
		t.Fatal("garbage must not decode")
	}
}

func TestTimeoutIsClassifiedAndNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	c := New(50 * time.Millisecond)
	start := time.Now()
	_, err := c.Get(context.Background(), srv.URL)
	if !IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("a timed-out request must not be retried, hits=%d", hits.Load())
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("timeout not enforced, took %v", time.Since(start))
	}
}

func TestRetriesOnceWhenBackendIsRestarting(t *testing.T) {
	// Grab a free port, close it so the first dial is refused, then start the
	// server on it while the client waits for its retry.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	c := New(2 * time.Second)
	c.RetryDelay = 300 * time.Millisecond
	go func() {
		time.Sleep(100 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })}
		go srv.Serve(ln)
		time.AfterFunc(2*time.Second, func() { srv.Close() })
	}()
	var out map[string]any
	if err := c.GetJSON(context.Background(), "http://"+addr+"/", &out); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
}

func TestStillFailsWhenBackendStaysDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	c := New(time.Second)
	c.RetryDelay = 10 * time.Millisecond
	_, err := c.Get(context.Background(), "http://"+addr+"/")
	if err == nil || IsTimeout(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
}

func TestResponseSizeIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", MaxBody+10)))
	}))
	defer srv.Close()
	if _, err := New(5*time.Second).Get(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response must be rejected, got %v", err)
	}
}

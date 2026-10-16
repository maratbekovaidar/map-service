package httpkit

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"map-service/internal/config"
)

func TestLimiterUnit(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(config.Limit{RPS: 2, Burst: 2}, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("burst request %d rejected", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > 600*time.Millisecond {
		t.Fatalf("third request: ok=%v wait=%v (rate 2/s -> ~500ms)", ok, wait)
	}
	// Rejections consume nothing: after the advertised wait the request passes.
	now = now.Add(wait)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("request after Retry-After must pass")
	}
	// Idle buckets are swept.
	if l.Len() != 1 {
		t.Fatalf("buckets = %d", l.Len())
	}
	now = now.Add(idleBucketTTL + 2*sweepEvery)
	l.Allow("b")
	if l.Len() != 1 { // only "b" survives
		t.Errorf("idle bucket not swept: %d", l.Len())
	}
	// Disabled limiter allows everything.
	off := NewLimiter(config.Limit{RPS: 0}, nil)
	if ok, _ := off.Allow("x"); !ok || off != nil {
		t.Error("RPS=0 must disable limiting")
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("127.0.0.1/32")}
	req := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"direct client ignores XFF (spoofing)", req("203.0.113.5:1000", "1.2.3.4"), "203.0.113.5"},
		{"behind trusted proxy", req("172.18.0.4:5555", "198.51.100.8"), "198.51.100.8"},
		{"client-supplied prefix is ignored", req("172.18.0.4:5555", "6.6.6.6, 198.51.100.8"), "198.51.100.8"},
		{"chain of trusted proxies", req("172.18.0.4:5555", "198.51.100.8, 172.18.0.9"), "198.51.100.8"},
		{"multiple XFF headers", req("172.18.0.4:5555", "6.6.6.6", "198.51.100.8"), "198.51.100.8"},
		{"garbage XFF falls back to the proxy", req("172.18.0.4:5555", "not-an-ip"), "172.18.0.4"},
		{"no XFF behind proxy", req("172.18.0.4:5555"), "172.18.0.4"},
		{"ipv6 client", req("[2001:db8::1]:443"), "2001:db8::1"},
		{"ipv4-mapped ipv6 remote", req("[::ffff:203.0.113.5]:80", "1.1.1.1"), "203.0.113.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClientIP(c.r, trusted); got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

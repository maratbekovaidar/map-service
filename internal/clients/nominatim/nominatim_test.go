package nominatim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"map-service/internal/geo"
)

func serve(t *testing.T, file string, status int) (*Client, *url.URL) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	got := &url.URL{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.URL
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, 2*time.Second), got
}

func TestSearchFixture(t *testing.T) {
	c, u := serve(t, "search_khan_shatyr.json", 200)
	bb, _ := geo.ParseBBox("71.04,50.74,71.96,51.46")
	res, err := c.Search(context.Background(), SearchParams{Query: "Хан Шатыр", Limit: 3, Languages: []string{"ru", "kk", "en"}, Bounds: &bb})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	p := res[0]
	if p.Name != "Хан Шатыр" || p.OSMType != "way" || p.OSMID == 0 || p.Type != "mall" {
		t.Errorf("place: %+v", p)
	}
	if p.Lat < 51.13 || p.Lat > 51.14 || p.Lon < 71.40 || p.Lon > 71.41 {
		t.Errorf("string coordinates must be parsed: %v,%v", p.Lat, p.Lon)
	}
	a := p.Address
	if a.Road != "проспект Туран" || a.HouseNumber != "37" || a.CityDistrict != "район Нура" || a.City != "Астана" || a.Postcode != "Z19Y4X2" {
		t.Errorf("address: %+v", a)
	}

	q := u.Query()
	if u.Path != "/search" || q.Get("q") != "Хан Шатыр" || q.Get("format") != "jsonv2" || q.Get("addressdetails") != "1" ||
		q.Get("limit") != "3" || q.Get("accept-language") != "ru,kk,en" || q.Get("bounded") != "1" {
		t.Errorf("request: %s", u)
	}
	// Nominatim wants left,top,right,bottom.
	if q.Get("viewbox") != "71.04,51.46,71.96,50.74" {
		t.Errorf("viewbox = %q", q.Get("viewbox"))
	}
}

func TestSearchEmpty(t *testing.T) {
	c, _ := serve(t, "search_empty.json", 200)
	res, err := c.Search(context.Background(), SearchParams{Query: "zzz"})
	if err != nil || len(res) != 0 {
		t.Fatalf("empty result must not be an error: %v %v", res, err)
	}
}

func TestReverse(t *testing.T) {
	c, u := serve(t, "reverse_mall.json", 200)
	p, err := c.Reverse(context.Background(), 51.1325, 71.4039, []string{"ru"})
	if err != nil {
		t.Fatal(err)
	}
	if p.OSMType != "node" || p.Name != "ForteBank" || p.Address.HouseNumber != "37" || p.Address.City != "Астана" {
		t.Errorf("place: %+v", p)
	}
	q := u.Query()
	if u.Path != "/reverse" || q.Get("lat") != "51.1325" || q.Get("lon") != "71.4039" || q.Get("zoom") != "18" || q.Get("accept-language") != "ru" {
		t.Errorf("request: %s", u)
	}

	// A street without a name keeps an empty Name (the gateway falls back to the address).
	c2, _ := serve(t, "reverse_street.json", 200)
	p2, err := c2.Reverse(context.Background(), 51.1283, 71.4306, nil)
	if err != nil || p2.Name != "" || p2.AddressType != "road" || p2.Address.CityDistrict != "район Есиль" {
		t.Errorf("street: %+v %v", p2, err)
	}
}

func TestReverseNotFoundIsHTTP200WithError(t *testing.T) {
	c, _ := serve(t, "reverse_error.json", 200)
	_, err := c.Reverse(context.Background(), 0.5, 0.5, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpstreamFailures(t *testing.T) {
	c, _ := serve(t, "search_empty.json", 503)
	if _, err := c.Search(context.Background(), SearchParams{Query: "x"}); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("HTTP 503 must be a plain error, got %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"place_id":1,"lat":"not-a-number","lon":"1"}]`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, time.Second).Search(context.Background(), SearchParams{Query: "x"}); err == nil {
		t.Error("garbage coordinates must be an error")
	}
}

func TestHealth(t *testing.T) {
	c, u := serve(t, "status.json", 200)
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u.Path != "/status" || u.Query().Get("format") != "json" {
		t.Errorf("request: %s", u)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":700,"message":"Database connection failed"}`))
	}))
	defer srv.Close()
	if err := New(srv.URL, time.Second).Health(context.Background()); err == nil {
		t.Error("non-zero status must fail the health check")
	}
}

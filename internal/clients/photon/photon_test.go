package photon

import (
	"context"
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
	c, u := serve(t, "api_khan_shatyr.json", 200)
	bb, _ := geo.ParseBBox("71.04,50.74,71.96,51.46")
	bias := geo.Point{Lon: 71.43, Lat: 51.13}
	res, err := c.Search(context.Background(), SearchParams{Query: "Хан Шатыр", Limit: 3, Lang: "ru", Bias: &bias, BBox: &bb})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("features = %d, want 3", len(res))
	}
	mall := res[1]
	if mall.Name != "Хан Шатыр" || mall.OSMType != "W" || mall.OSMID != 460703779 || mall.OSMKey != "shop" || mall.OSMValue != "mall" ||
		mall.HouseNumber != "37" || mall.Street != "проспект Туран" || mall.District != "район Нура" || mall.City != "Астана" || mall.Postcode != "Z19Y4X2" {
		t.Errorf("mall: %+v", mall)
	}
	if mall.Lon != 71.4038607 || mall.Lat != 51.1325048 {
		t.Errorf("GeoJSON order is [lon, lat]: got lon=%v lat=%v", mall.Lon, mall.Lat)
	}

	q := u.Query()
	if u.Path != "/api" || q.Get("q") != "Хан Шатыр" || q.Get("limit") != "3" || q.Get("lang") != "ru" ||
		q.Get("lat") != "51.13" || q.Get("lon") != "71.43" || q.Get("bbox") != "71.04,50.74,71.96,51.46" {
		t.Errorf("request: %s", u)
	}
}

func TestAddressObjectsHaveNoName(t *testing.T) {
	c, _ := serve(t, "api_street_number.json", 200)
	res, err := c.Search(context.Background(), SearchParams{Query: "проспект Туран 37"})
	if err != nil || len(res) == 0 {
		t.Fatal(res, err)
	}
	if res[0].Name != "" || res[0].HouseNumber != "37" || res[0].Street != "проспект Туран" || res[0].OSMValue != "house" {
		t.Errorf("address feature: %+v", res[0])
	}
}

func TestEmptyAndOptionalParams(t *testing.T) {
	c, u := serve(t, "api_empty.json", 200)
	res, err := c.Search(context.Background(), SearchParams{Query: "zzz"})
	if err != nil || len(res) != 0 {
		t.Fatalf("empty result: %v %v", res, err)
	}
	for _, k := range []string{"lat", "lon", "bbox", "lang", "limit"} {
		if u.Query().Has(k) {
			t.Errorf("unset parameter %q must not be sent", k)
		}
	}
}

func TestNonPointFeaturesAreSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"type":"FeatureCollection","features":[
		 {"type":"Feature","geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]},"properties":{"name":"poly"}},
		 {"type":"Feature","geometry":{"type":"Point","coordinates":[71.4,51.1]},"properties":{"name":"pt","osm_type":"N","osm_id":1}}]}`))
	}))
	defer srv.Close()
	res, err := New(srv.URL, time.Second).Search(context.Background(), SearchParams{Query: "x"})
	if err != nil || len(res) != 1 || res[0].Name != "pt" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestBadRequestAndHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(`{"status":"Ok"}`))
			return
		}
		http.Error(w, `{"message":"Language is not supported: 'xx'"}`, http.StatusBadRequest) // real Photon behaviour
	}))
	defer srv.Close()
	c := New(srv.URL, time.Second)
	if _, err := c.Search(context.Background(), SearchParams{Query: "x", Lang: "xx"}); err == nil {
		t.Error("HTTP 400 must be an error")
	}
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health: %v", err)
	}

	c2, u := serve(t, "status.json", 200)
	if err := c2.Health(context.Background()); err != nil || u.Path != "/status" {
		t.Errorf("fixture health: %v %s", err, u)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"Error"}`)) }))
	defer down.Close()
	if err := New(down.URL, time.Second).Health(context.Background()); err == nil {
		t.Error(`status "Error" must fail the health check`)
	}
}

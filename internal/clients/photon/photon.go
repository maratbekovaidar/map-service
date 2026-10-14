// Package photon is a thin client for the Photon search API (/api, /status).
package photon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"map-service/internal/clients/httpx"
	"map-service/internal/geo"
)

// Client talks to one Photon instance.
type Client struct {
	base string
	http *httpx.Client
}

// New creates a client for the server at baseURL.
func New(baseURL string, timeout time.Duration) *Client {
	return NewWithHTTP(baseURL, httpx.New(timeout))
}

// NewWithHTTP is New with a caller-provided HTTP layer (tests).
func NewWithHTTP(baseURL string, h *httpx.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), http: h}
}

// Feature is one Photon result, flattened from its GeoJSON form.
type Feature struct {
	OSMType     string // N, W or R
	OSMID       int64
	OSMKey      string // main OSM tag key (amenity, shop, place, ...)
	OSMValue    string // main OSM tag value (cafe, mall, house, ...)
	Type        string // Photon object class: house, street, city, ...
	Name        string // empty for plain addresses
	HouseNumber string
	Street      string
	District    string
	City        string
	Postcode    string
	Lon, Lat    float64
}

// SearchParams describes an /api request.
type SearchParams struct {
	Query string
	Limit int
	Lang  string     // must be one of the languages Photon was imported with
	Bias  *geo.Point // soft location bias
	BBox  *geo.BBox  // hard restriction
}

// Search runs a (prefix-friendly) search. An empty result is not an error.
func (c *Client) Search(ctx context.Context, p SearchParams) ([]Feature, error) {
	q := url.Values{}
	q.Set("q", p.Query)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Lang != "" {
		q.Set("lang", p.Lang)
	}
	if p.Bias != nil {
		q.Set("lat", strconv.FormatFloat(p.Bias.Lat, 'f', -1, 64))
		q.Set("lon", strconv.FormatFloat(p.Bias.Lon, 'f', -1, 64))
	}
	if p.BBox != nil {
		q.Set("bbox", p.BBox.String()) // minLon,minLat,maxLon,maxLat
	}
	var raw struct {
		Features []struct {
			Geometry struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
			Properties struct {
				OSMType     string `json:"osm_type"`
				OSMID       int64  `json:"osm_id"`
				OSMKey      string `json:"osm_key"`
				OSMValue    string `json:"osm_value"`
				Type        string `json:"type"`
				Name        string `json:"name"`
				HouseNumber string `json:"housenumber"`
				Street      string `json:"street"`
				District    string `json:"district"`
				City        string `json:"city"`
				Postcode    string `json:"postcode"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := c.http.GetJSON(ctx, c.base+"/api?"+q.Encode(), &raw); err != nil {
		return nil, err
	}
	out := make([]Feature, 0, len(raw.Features))
	for _, f := range raw.Features {
		if f.Geometry.Type != "Point" {
			continue // only point results are meaningful for the gateway
		}
		var xy []float64
		if err := json.Unmarshal(f.Geometry.Coordinates, &xy); err != nil || len(xy) < 2 {
			return nil, fmt.Errorf("photon: bad coordinates %s", f.Geometry.Coordinates)
		}
		pr := f.Properties
		out = append(out, Feature{
			OSMType: pr.OSMType, OSMID: pr.OSMID, OSMKey: pr.OSMKey, OSMValue: pr.OSMValue,
			Type: pr.Type, Name: pr.Name, HouseNumber: pr.HouseNumber, Street: pr.Street,
			District: pr.District, City: pr.City, Postcode: pr.Postcode,
			Lon: xy[0], Lat: xy[1],
		})
	}
	return out, nil
}

// Health checks that Photon and its index answer.
func (c *Client) Health(ctx context.Context) error {
	var s struct {
		Status string `json:"status"`
	}
	if err := c.http.GetJSON(ctx, c.base+"/status", &s); err != nil {
		return err
	}
	if !strings.EqualFold(s.Status, "ok") {
		return fmt.Errorf("photon status %q", s.Status)
	}
	return nil
}

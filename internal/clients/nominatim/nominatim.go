// Package nominatim is a thin client for the Nominatim HTTP API
// (/search, /reverse, /status).
package nominatim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"map-service/internal/clients/httpx"
	"map-service/internal/geo"
)

// ErrNotFound means Nominatim has no object at the requested location.
var ErrNotFound = errors.New("nominatim: nothing found")

// Client talks to one Nominatim instance.
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

// Address is the structured address Nominatim returns (addressdetails=1).
// Only the keys the gateway maps are declared.
type Address struct {
	Road          string `json:"road"`
	Pedestrian    string `json:"pedestrian"`
	Footway       string `json:"footway"`
	HouseNumber   string `json:"house_number"`
	Neighbourhood string `json:"neighbourhood"`
	Quarter       string `json:"quarter"`
	Suburb        string `json:"suburb"`
	CityDistrict  string `json:"city_district"`
	District      string `json:"district"`
	City          string `json:"city"`
	Town          string `json:"town"`
	Village       string `json:"village"`
	Municipality  string `json:"municipality"`
	County        string `json:"county"`
	State         string `json:"state"`
	Postcode      string `json:"postcode"`
	Country       string `json:"country"`
	CountryCode   string `json:"country_code"`
}

// Place is one Nominatim result (search or reverse), jsonv2 format.
type Place struct {
	PlaceID     int64
	OSMType     string // node, way, relation
	OSMID       int64
	Lat, Lon    float64
	Category    string
	Type        string
	AddressType string
	Name        string
	DisplayName string
	Address     Address
}

// UnmarshalJSON parses the string-typed coordinates.
func (p *Place) UnmarshalJSON(b []byte) error {
	var raw struct {
		PlaceID     int64   `json:"place_id"`
		OSMType     string  `json:"osm_type"`
		OSMID       int64   `json:"osm_id"`
		Lat         string  `json:"lat"`
		Lon         string  `json:"lon"`
		Category    string  `json:"category"`
		Type        string  `json:"type"`
		AddressType string  `json:"addresstype"`
		Name        string  `json:"name"`
		DisplayName string  `json:"display_name"`
		Address     Address `json:"address"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	lat, err := strconv.ParseFloat(raw.Lat, 64)
	if err != nil {
		return fmt.Errorf("nominatim: bad lat %q: %w", raw.Lat, err)
	}
	lon, err := strconv.ParseFloat(raw.Lon, 64)
	if err != nil {
		return fmt.Errorf("nominatim: bad lon %q: %w", raw.Lon, err)
	}
	*p = Place{
		PlaceID: raw.PlaceID, OSMType: raw.OSMType, OSMID: raw.OSMID, Lat: lat, Lon: lon,
		Category: raw.Category, Type: raw.Type, AddressType: raw.AddressType,
		Name: raw.Name, DisplayName: raw.DisplayName, Address: raw.Address,
	}
	return nil
}

// SearchParams describes a /search request.
type SearchParams struct {
	Query     string
	Limit     int
	Languages []string  // accept-language, in priority order
	Bounds    *geo.BBox // restrict (bounded=1) results to this box
}

// Search runs a free-text search. An empty result is not an error.
func (c *Client) Search(ctx context.Context, p SearchParams) ([]Place, error) {
	q := url.Values{}
	q.Set("q", p.Query)
	q.Set("format", "jsonv2")
	q.Set("addressdetails", "1")
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if len(p.Languages) > 0 {
		q.Set("accept-language", strings.Join(p.Languages, ","))
	}
	if p.Bounds != nil {
		q.Set("viewbox", p.Bounds.Viewbox())
		q.Set("bounded", "1")
	}
	var out []Place
	if err := c.http.GetJSON(ctx, c.base+"/search?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Reverse returns the object at (lat, lon), or ErrNotFound.
func (c *Client) Reverse(ctx context.Context, lat, lon float64, languages []string) (*Place, error) {
	q := url.Values{}
	q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
	q.Set("lon", strconv.FormatFloat(lon, 'f', -1, 64))
	q.Set("format", "jsonv2")
	q.Set("addressdetails", "1")
	q.Set("zoom", "18") // building level
	if len(languages) > 0 {
		q.Set("accept-language", strings.Join(languages, ","))
	}
	b, err := c.http.Get(ctx, c.base+"/reverse?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// "Unable to geocode" arrives as HTTP 200 {"error": "..."}.
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("nominatim: decode reverse response: %w", err)
	}
	if probe.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, probe.Error)
	}
	var out Place
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("nominatim: decode reverse place: %w", err)
	}
	return &out, nil
}

// Status is the answer of /status?format=json.
type Status struct {
	Status          int    `json:"status"`
	Message         string `json:"message"`
	DataUpdated     string `json:"data_updated"`
	SoftwareVersion string `json:"software_version"`
}

// Health checks that Nominatim and its database answer.
func (c *Client) Health(ctx context.Context) error {
	var s Status
	if err := c.http.GetJSON(ctx, c.base+"/status?format=json", &s); err != nil {
		return err
	}
	if s.Status != 0 {
		return fmt.Errorf("nominatim status %d: %s", s.Status, s.Message)
	}
	return nil
}

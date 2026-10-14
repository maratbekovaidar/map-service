package httpapi

import (
	"encoding/json"
	"strconv"
	"strings"

	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/geo"
)

// genericTypes are OSM values that say nothing about what a place is.
var genericTypes = map[string]bool{"": true, "yes": true, "road": true}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// streetNumber renders "street, number" for objects that have no name.
func streetNumber(street, number string) string {
	switch {
	case street != "" && number != "":
		return street + ", " + number
	case street != "":
		return street
	default:
		return number
	}
}

var photonOSMType = map[string]string{"N": "node", "W": "way", "R": "relation"}

// fromPhoton maps a Photon feature to the unified Place.
func fromPhoton(f photon.Feature) Place {
	id := ""
	if t, ok := photonOSMType[strings.ToUpper(f.OSMType)]; ok && f.OSMID != 0 {
		id = t + "/" + strconv.FormatInt(f.OSMID, 10)
	}
	typ := f.OSMValue
	if genericTypes[typ] {
		typ = f.Type
	}
	return Place{
		ID:   id,
		Name: firstNonEmpty(f.Name, streetNumber(f.Street, f.HouseNumber), f.City),
		Address: Address{
			Street: f.Street, HouseNumber: f.HouseNumber, District: f.District, City: f.City, Postcode: f.Postcode,
		},
		Lat: f.Lat, Lon: f.Lon, Type: typ, Source: SourcePhoton,
	}
}

// fromNominatim maps a Nominatim result to the unified Place.
func fromNominatim(p nominatim.Place) Place {
	a := p.Address
	street := firstNonEmpty(a.Road, a.Pedestrian, a.Footway)
	id := "nominatim/" + strconv.FormatInt(p.PlaceID, 10)
	if p.OSMType != "" && p.OSMID != 0 {
		id = p.OSMType + "/" + strconv.FormatInt(p.OSMID, 10)
	}
	typ := p.Type
	if genericTypes[typ] {
		typ = firstNonEmpty(p.AddressType, p.Category)
	}
	display, _, _ := strings.Cut(p.DisplayName, ",")
	return Place{
		ID:   id,
		Name: firstNonEmpty(p.Name, streetNumber(street, a.HouseNumber), strings.TrimSpace(display)),
		Address: Address{
			Street:      street,
			HouseNumber: a.HouseNumber,
			District:    firstNonEmpty(a.CityDistrict, a.Suburb, a.District, a.Neighbourhood, a.Quarter),
			City:        firstNonEmpty(a.City, a.Town, a.Village, a.Municipality),
			Postcode:    a.Postcode,
		},
		Lat: p.Lat, Lon: p.Lon, Type: typ, Source: SourceNominatim,
	}
}

// keepInside drops places outside the service area, preserving order.
func keepInside(places []Place, bbox geo.BBox) []Place {
	out := places[:0:0]
	for _, p := range places {
		if bbox.Contains(geo.Point{Lon: p.Lon, Lat: p.Lat}) {
			out = append(out, p)
		}
	}
	return out
}

// geometryJSON renders an OSRM geometry as a JSON string (polyline) or a
// GeoJSON LineString, according to what was requested.
func geometryJSON(g osrm.RouteGeometry, want osrm.Geometry) json.RawMessage {
	if want == osrm.GeoJSON {
		coords := g.Line
		if coords == nil {
			coords = osrm.LineString{}
		}
		b, _ := json.Marshal(struct {
			Type        string          `json:"type"`
			Coordinates osrm.LineString `json:"coordinates"`
		}{"LineString", coords})
		return b
	}
	b, _ := json.Marshal(g.Encoded)
	return b
}

// fromOSRM maps an OSRM answer to the API route response.
func fromOSRM(res *osrm.RouteResponse, want osrm.Geometry) RouteResponse {
	out := RouteResponse{Routes: make([]Route, 0, len(res.Routes)), Waypoints: make([]Waypoint, 0, len(res.Waypoints))}
	for _, r := range res.Routes {
		route := Route{
			DistanceM: r.Distance, DurationS: r.Duration,
			Geometry: geometryJSON(r.Geometry, want),
			Legs:     make([]Leg, 0, len(r.Legs)),
		}
		for _, l := range r.Legs {
			leg := Leg{DistanceM: l.Distance, DurationS: l.Duration, Summary: l.Summary}
			for _, s := range l.Steps {
				leg.Steps = append(leg.Steps, Step{
					DistanceM: s.Distance, DurationS: s.Duration, Name: s.Name,
					Maneuver: Maneuver{Type: s.Maneuver.Type, Modifier: s.Maneuver.Modifier, Lon: s.Maneuver.Location[0], Lat: s.Maneuver.Location[1]},
					Geometry: geometryJSON(s.Geometry, want),
				})
			}
			route.Legs = append(route.Legs, leg)
		}
		out.Routes = append(out.Routes, route)
	}
	for _, w := range res.Waypoints {
		out.Waypoints = append(out.Waypoints, Waypoint{Name: w.Name, Lon: w.Location[0], Lat: w.Location[1]})
	}
	return out
}

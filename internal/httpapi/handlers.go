package httpapi

import (
	"context"
	"errors"
	"net/http"

	"map-service/internal/cache"
	"map-service/internal/clients/nominatim"
	"map-service/internal/clients/osrm"
	"map-service/internal/clients/photon"
	"map-service/internal/geo"
)

// GET /v1/autocomplete - Photon first, Nominatim when Photon fails or has nothing.
func (s *Server) handleAutocomplete(w http.ResponseWriter, r *http.Request) error {
	vals, e := queryValues(r)
	if e != nil {
		return e
	}
	q, e := s.val.query(vals)
	if e != nil {
		return e
	}
	limit, e := s.val.limit(vals)
	if e != nil {
		return e
	}
	lang, e := s.val.lang(vals)
	if e != nil {
		return e
	}
	bias, e := s.val.optionalBias(vals)
	if e != nil {
		return e
	}

	ctx := r.Context()
	var biasLat, biasLon *float64
	if bias != nil {
		biasLat, biasLon = &bias.Lat, &bias.Lon
	}
	key := cache.AutocompleteKey(s.version(), q, limit, lang, biasLat, biasLon)
	var resp PlacesResponse
	if s.cache.GetJSON(ctx, key, &resp) {
		writeJSON(w, http.StatusOK, resp)
		return nil
	}

	places, cacheable, err := s.autocomplete(ctx, q, limit, lang, bias)
	if err != nil {
		return err
	}
	resp = PlacesResponse{Places: places}
	if cacheable {
		s.cache.SetJSON(ctx, key, resp, s.cfg.TTL.Autocomplete)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// autocomplete returns the places and whether the answer may be cached. An
// answer produced by the fallback because Photon *failed* is not cached, so a
// brief Photon outage does not pin lower-quality results for a whole TTL.
func (s *Server) autocomplete(ctx context.Context, q string, limit int, lang string, bias *geo.Point) ([]Place, bool, error) {
	log := loggerFrom(ctx)
	bbox := s.cfg.BBox

	feats, photonErr := s.photon.Search(ctx, photon.SearchParams{Query: q, Limit: limit, Lang: lang, Bias: bias, BBox: &bbox})
	var places []Place
	if photonErr == nil {
		for _, f := range feats {
			places = append(places, fromPhoton(f))
		}
		places = keepInside(places, bbox)
		if len(places) > 0 {
			return places, true, nil
		}
	} else {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		log.Warn("photon failed, falling back to nominatim", "err", photonErr)
	}

	found, nominatimErr := s.nominatim.Search(ctx, nominatim.SearchParams{
		Query: q, Limit: limit, Languages: s.val.langPriority(lang), Bounds: &bbox,
	})
	if nominatimErr != nil {
		if photonErr != nil {
			return nil, false, upstreamError("geocoding", nominatimErr) // both backends failed
		}
		// Photon answered (just without results); the fallback is best effort.
		log.Warn("nominatim fallback failed after an empty photon answer", "err", nominatimErr)
		return []Place{}, true, nil
	}
	places = make([]Place, 0, len(found))
	for _, p := range found {
		places = append(places, fromNominatim(p))
	}
	return keepInside(places, bbox), photonErr == nil, nil
}

// GET /v1/geocode
func (s *Server) handleGeocode(w http.ResponseWriter, r *http.Request) error {
	vals, e := queryValues(r)
	if e != nil {
		return e
	}
	q, e := s.val.query(vals)
	if e != nil {
		return e
	}
	limit, e := s.val.limit(vals)
	if e != nil {
		return e
	}
	lang, e := s.val.lang(vals)
	if e != nil {
		return e
	}
	ctx := r.Context()
	key := cache.GeocodeKey(s.version(), q, limit, lang)
	var resp PlacesResponse
	if s.cache.GetJSON(ctx, key, &resp) {
		writeJSON(w, http.StatusOK, resp)
		return nil
	}
	bbox := s.cfg.BBox
	found, err := s.nominatim.Search(ctx, nominatim.SearchParams{Query: q, Limit: limit, Languages: s.val.langPriority(lang), Bounds: &bbox})
	if err != nil {
		return upstreamError("geocoding", err)
	}
	places := make([]Place, 0, len(found))
	for _, p := range found {
		places = append(places, fromNominatim(p))
	}
	resp = PlacesResponse{Places: keepInside(places, bbox)}
	s.cache.SetJSON(ctx, key, resp, s.cfg.TTL.Geocode)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// GET /v1/reverse
func (s *Server) handleReverse(w http.ResponseWriter, r *http.Request) error {
	vals, e := queryValues(r)
	if e != nil {
		return e
	}
	pt, e := s.val.point(vals)
	if e != nil {
		return e
	}
	lang, e := s.val.lang(vals)
	if e != nil {
		return e
	}
	// Query upstream with the same 5-decimal rounding the cache key uses, so a
	// cached answer is exactly what a fresh request for that key would return.
	lat, lon := geo.Round5(pt.Lat), geo.Round5(pt.Lon)

	ctx := r.Context()
	key := cache.ReverseKey(s.version(), lat, lon, lang)
	var place Place
	if s.cache.GetJSON(ctx, key, &place) {
		writeJSON(w, http.StatusOK, place)
		return nil
	}
	found, err := s.nominatim.Reverse(ctx, lat, lon, s.val.langPriority(lang))
	switch {
	case errors.Is(err, nominatim.ErrNotFound):
		return notFound("nothing found at this location")
	case err != nil:
		return upstreamError("geocoding", err)
	}
	place = fromNominatim(*found)
	if !s.cfg.BBox.Contains(geo.Point{Lon: place.Lon, Lat: place.Lat}) {
		return notFound("nothing found at this location")
	}
	s.cache.SetJSON(ctx, key, place, s.cfg.TTL.Reverse)
	writeJSON(w, http.StatusOK, place)
	return nil
}

// GET /v1/route
func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) error {
	vals, e := queryValues(r)
	if e != nil {
		return e
	}
	profile, e := s.val.profile(vals)
	if e != nil {
		return e
	}
	points, e := s.val.points(vals.Get("points"))
	if e != nil {
		return e
	}
	alternatives, e := parseBool(vals, "alternatives")
	if e != nil {
		return e
	}
	steps, e := parseBool(vals, "steps")
	if e != nil {
		return e
	}
	geometry, e := s.val.geometry(vals)
	if e != nil {
		return e
	}
	client, ok := s.osrm[profile]
	if !ok {
		return internalError(errors.New("no OSRM client for profile " + profile))
	}
	for i := range points { // see handleReverse: cache key and request agree
		points[i] = geo.Point{Lon: geo.Round5(points[i].Lon), Lat: geo.Round5(points[i].Lat)}
	}

	ctx := r.Context()
	key := cache.RouteKey(s.version(), profile, points, alternatives, steps, string(geometry))
	var resp RouteResponse
	if s.cache.GetJSON(ctx, key, &resp) {
		writeJSON(w, http.StatusOK, resp)
		return nil
	}
	res, err := client.Route(ctx, osrm.RouteParams{Profile: profile, Points: points, Alternatives: alternatives, Steps: steps, Geometry: geometry})
	var apiErr *osrm.APIError
	switch {
	case errors.Is(err, osrm.ErrNoRoute):
		return noRoute()
	case errors.As(err, &apiErr):
		// OSRM rejected a request we validated: a bug or a limit mismatch, not the client's fault.
		return &Error{Status: http.StatusBadGateway, Code: CodeUpstreamUnavailable, Message: "routing service rejected the request", Err: err}
	case err != nil:
		return upstreamError("routing", err)
	}
	resp = fromOSRM(res, geometry)
	s.cache.SetJSON(ctx, key, resp, s.cfg.TTL.Route)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

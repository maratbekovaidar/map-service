package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"map-service/internal/styles"
)

// StyleInfo describes one installed map style in GET /v1/styles.
type StyleInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Default     bool   `json:"default"`
}

// StylesResponse is the body of GET /v1/styles and PUT /v1/styles/default.
type StylesResponse struct {
	Default string      `json:"default"`
	Styles  []StyleInfo `json:"styles"`
}

// SetDefaultStyleRequest is the body of PUT /v1/styles/default.
type SetDefaultStyleRequest struct {
	ID string `json:"id"`
}

// requireAdmin lets through only requests with one of ADMIN_API_KEYS. With no
// admin keys configured the endpoint is switched off (403), whatever
// AUTH_DISABLED says: it changes what every client sees.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.AdminKeys) == 0 {
			writeError(w, forbidden("switching the default style is disabled: set ADMIN_API_KEYS"))
			return
		}
		id, ok := s.admin.Check(r.Header.Get("X-API-Key"))
		if !ok {
			loggerFrom(r.Context()).Info("rejected request: not an admin key")
			writeError(w, &Error{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: "an admin API key (ADMIN_API_KEYS) is required"})
			return
		}
		setKeyID(r.Context(), id)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) stylesResponse(r *http.Request) (*StylesResponse, error) {
	list, err := s.styles.List()
	if err != nil {
		return nil, internalError(err)
	}
	def, err := s.styles.Default()
	if err != nil && !errors.Is(err, styles.ErrUnknown) {
		return nil, internalError(err)
	}
	base := s.static.publicBase(r)
	resp := &StylesResponse{Default: def, Styles: make([]StyleInfo, 0, len(list))}
	for _, st := range list {
		resp.Styles = append(resp.Styles, StyleInfo{
			ID: st.ID, Name: st.Name, Description: st.Description,
			URL:     base + "/tiles/styles/" + st.ID + ".json",
			Default: st.ID == def,
		})
	}
	return resp, nil
}

// GET /v1/styles
func (s *Server) handleStyles(w http.ResponseWriter, r *http.Request) error {
	resp, err := s.stylesResponse(r)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// PUT /v1/styles/default
func (s *Server) handleSetDefaultStyle(w http.ResponseWriter, r *http.Request) error {
	var req SetDefaultStyleRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest(`body must be JSON like {"id": "liberty"}`)
	}
	id := strings.TrimSpace(req.ID)
	if !styles.ValidID(id) {
		return badRequest("id must be a style id from GET /v1/styles")
	}
	prev, _ := s.styles.Default()
	if err := s.styles.SetDefault(id); errors.Is(err, styles.ErrUnknown) {
		return badRequest("unknown style %q, see GET /v1/styles", id)
	} else if err != nil {
		return internalError(err)
	}
	loggerFrom(r.Context()).Info("default style changed", "from", prev, "to", id, "persisted", s.styles.Persistent())
	resp, err := s.stylesResponse(r)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// serveStyle writes style document b with the public base URL filled in.
func (s *Server) serveStyle(w http.ResponseWriter, r *http.Request, b []byte, cacheControl string) {
	esc, _ := json.Marshal(s.static.publicBase(r))
	b = bytes.ReplaceAll(b, []byte(publicURLPlaceholder), bytes.Trim(esc, `"`))
	serveBytes(w, r, "application/json; charset=utf-8", cacheControl, b)
}

// GET /tiles/style.json - the default style. It can change at runtime, so
// clients revalidate it on every use (cheap: ETag / 304).
func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) error {
	id, err := s.styles.Default()
	if errors.Is(err, styles.ErrUnknown) {
		return notFound("no default style is installed")
	}
	if err != nil {
		return internalError(err)
	}
	b, err := s.styles.Read(id)
	if errors.Is(err, styles.ErrUnknown) {
		return notFound("no default style is installed")
	}
	if err != nil {
		return internalError(err)
	}
	s.serveStyle(w, r, b, "no-cache")
	return nil
}

// GET /tiles/styles/{id}.json - one style by id. Revalidated like the
// default style, so an edited theme shows up on the next reload.
func (s *Server) handleStyleByID(w http.ResponseWriter, r *http.Request) error {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".json")
	if !ok || !styles.ValidID(id) {
		return notFound("no such style")
	}
	b, err := s.styles.Read(id)
	if errors.Is(err, styles.ErrUnknown) {
		return notFound("no such style %q, see GET /v1/styles", id)
	}
	if err != nil {
		return internalError(err)
	}
	s.serveStyle(w, r, b, "no-cache")
	return nil
}

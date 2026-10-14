package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"map-service/internal/config"
	"map-service/internal/dataset"
	"map-service/internal/tiles"
)

const publicURLPlaceholder = "{{PUBLIC_BASE_URL}}"

var (
	fontStackRe = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)
	glyphFileRe = regexp.MustCompile(`^[0-9]{1,6}-[0-9]{1,6}\.pbf$`)
	spriteRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}(@2x)?\.(json|png)$`)
	vendorFiles = map[string]string{
		"maplibre-gl.js":  "text/javascript; charset=utf-8",
		"maplibre-gl.css": "text/css; charset=utf-8",
	}
	hostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:]+\](:[0-9]{1,5})?$`)
)

// staticFiles serves the style, glyphs, sprites and the preview page from
// STATIC_DIR (baked into the gateway image).
type staticFiles struct {
	dir       string
	publicURL string
	trusted   []netip.Prefix
	maxAge    time.Duration
}

func newStaticFiles(cfg *config.Gateway) *staticFiles {
	return &staticFiles{dir: cfg.StaticDir, publicURL: cfg.PublicBaseURL, trusted: cfg.TrustedProxies, maxAge: cfg.TilesMaxAge}
}

// publicBase is the externally visible base URL written into style.json:
// PUBLIC_BASE_URL when configured, else derived from the request (forwarded
// headers are honoured only from trusted proxies, and the host is sanity-checked
// because it ends up inside a JSON document).
func (f *staticFiles) publicBase(r *http.Request) string {
	if f.publicURL != "" {
		return f.publicURL
	}
	proxied := isTrusted(remoteAddr(r.RemoteAddr), f.trusted)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if proxied {
		if v := r.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
			scheme = v
		}
		if v := r.Header.Get("X-Forwarded-Host"); v != "" {
			host = strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	if !hostRe.MatchString(host) {
		host = "localhost"
	}
	return scheme + "://" + host
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches implements If-None-Match for a single strong validator.
func etagMatches(r *http.Request, etag string) bool {
	for _, v := range r.Header.Values("If-None-Match") {
		for _, cand := range strings.Split(v, ",") {
			cand = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cand), "W/"))
			if cand == etag || cand == "*" {
				return true
			}
		}
	}
	return false
}

// serveBytes writes body with an ETag and Cache-Control, answering 304 when
// the client already has it.
func serveBytes(w http.ResponseWriter, r *http.Request, contentType, cacheControl string, body []byte) {
	etag := etagOf(body)
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", cacheControl)
	if etagMatches(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func readStatic(dir string, parts ...string) ([]byte, error) {
	return os.ReadFile(filepath.Join(append([]string{dir}, parts...)...))
}

// GET /tiles/style.json
func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) error {
	b, err := readStatic(s.static.dir, "style.json")
	if errors.Is(err, fs.ErrNotExist) {
		return notFound("style.json is not installed")
	}
	if err != nil {
		return internalError(err)
	}
	esc, _ := json.Marshal(s.static.publicBase(r))
	b = bytes.ReplaceAll(b, []byte(publicURLPlaceholder), bytes.Trim(esc, `"`))
	serveBytes(w, r, "application/json; charset=utf-8", "public, max-age=300", b)
	return nil
}

// GET /tiles/preview.html
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) error {
	b, err := readStatic(s.static.dir, "preview.html")
	if errors.Is(err, fs.ErrNotExist) {
		return notFound("preview.html is not installed")
	}
	if err != nil {
		return internalError(err)
	}
	serveBytes(w, r, "text/html; charset=utf-8", "public, max-age=300", b)
	return nil
}

// GET /tiles/fonts/{fontstack}/{range}.pbf
//
// A font stack that exists but lacks the requested glyph range gets an empty
// (valid) glyph message instead of a 404: renderers then simply draw no glyphs
// from that range and do not log errors for scripts the city does not use.
func (s *Server) handleFont(w http.ResponseWriter, r *http.Request) error {
	stack, file := r.PathValue("fontstack"), r.PathValue("file")
	if !fontStackRe.MatchString(stack) || !glyphFileRe.MatchString(file) {
		return notFound("no such glyph range")
	}
	b, err := readStatic(s.static.dir, "fonts", stack, file)
	if errors.Is(err, fs.ErrNotExist) {
		if _, derr := os.Stat(filepath.Join(s.static.dir, "fonts", stack)); derr != nil {
			return notFound("no such font stack")
		}
		serveBytes(w, r, "application/x-protobuf", "public, max-age=86400", nil)
		return nil
	}
	if err != nil {
		return internalError(err)
	}
	serveBytes(w, r, "application/x-protobuf", "public, max-age=604800", b)
	return nil
}

// GET /tiles/sprites/{name}
func (s *Server) handleSprite(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	if !spriteRe.MatchString(name) {
		return notFound("no such sprite")
	}
	b, err := readStatic(s.static.dir, "sprites", name)
	if errors.Is(err, fs.ErrNotExist) {
		return notFound("no such sprite")
	}
	if err != nil {
		return internalError(err)
	}
	ct := "application/json; charset=utf-8"
	if strings.HasSuffix(name, ".png") {
		ct = "image/png"
	}
	serveBytes(w, r, ct, "public, max-age=86400", b)
	return nil
}

// GET /tiles/vendor/{file} - the MapLibre GL JS files used by preview.html.
func (s *Server) handleVendor(w http.ResponseWriter, r *http.Request) error {
	return s.serveVendor(w, r, vendorFiles)
}

// serveVendor serves the {file} of the request from STATIC_DIR/vendor when it
// is one of allowed (file name -> content type); nothing else is reachable.
func (s *Server) serveVendor(w http.ResponseWriter, r *http.Request, allowed map[string]string) error {
	name := r.PathValue("file")
	ct, ok := allowed[name]
	if !ok {
		return notFound("no such file")
	}
	b, err := readStatic(s.static.dir, "vendor", name)
	if errors.Is(err, fs.ErrNotExist) {
		return notFound("no such file")
	}
	if err != nil {
		return internalError(err)
	}
	serveBytes(w, r, ct, "public, max-age=86400", b)
	return nil
}

func parseTileIndex(s string) (int, bool) {
	if s == "" || len(s) > 7 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// GET /tiles/{z}/{x}/{y}.mvt
func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) error {
	ys, ok := strings.CutSuffix(r.PathValue("file"), ".mvt")
	z, okZ := parseTileIndex(r.PathValue("z"))
	x, okX := parseTileIndex(r.PathValue("x"))
	y, okY := parseTileIndex(ys)
	if !ok || !okZ || !okX || !okY || !tiles.ValidTile(z, x, y) {
		return badRequest("tile coordinates must be /tiles/{z}/{x}/{y}.mvt with 0 <= z <= %d and x, y inside the zoom's grid", tiles.MaxZoom)
	}
	res := s.tiles.Tile(r.Context(), dataset.CurrentLink, z, x, y)
	cacheControl := "public, max-age=" + strconv.Itoa(int(s.static.maxAge/time.Second))
	h := w.Header()
	switch res.Status {
	case http.StatusOK:
		etag := res.Headers["ETag"]
		h.Set("ETag", etag)
		h.Set("Cache-Control", cacheControl)
		if etag != "" && etagMatches(r, etag) {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
		h.Set("Content-Type", "application/vnd.mapbox-vector-tile")
		if enc := res.Headers["Content-Encoding"]; enc != "" {
			h.Set("Content-Encoding", enc)
		}
		h.Set("Content-Length", strconv.Itoa(len(res.Body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.Body)
		return nil
	case http.StatusNoContent: // valid tile without data
		h.Set("Cache-Control", cacheControl)
		w.WriteHeader(http.StatusNoContent)
		return nil
	case 499: // client went away
		return context.Canceled
	default:
		return &Error{Status: http.StatusBadGateway, Code: CodeUpstreamUnavailable, Message: "tile archive is unavailable",
			Err: errors.New("tiles: status " + strconv.Itoa(res.Status) + ": " + strings.TrimSpace(string(res.Body)))}
	}
}

// Package tiles serves vector tiles from the PMTiles archive of the live (or
// any named) data version, using the embedded go-pmtiles server.
package tiles

import (
	"context"
	"fmt"
	stdlog "log"
	"log/slog"
	"strconv"
	"strings"

	"github.com/protomaps/go-pmtiles/pmtiles"

	"map-service/internal/dataset"
)

// MaxZoom is the highest zoom level stored in the archive (tilemaker config).
// Clients overzoom beyond it.
const MaxZoom = 14

// Result is the outcome of a tile lookup.
type Result struct {
	Status  int // 200 tile, 204 empty tile, 404 archive missing, 5xx I/O
	Headers map[string]string
	Body    []byte
}

// Server reads tiles from archives below a data root.
type Server struct {
	srv *pmtiles.Server
	// archive is the archive name without ".pmtiles", relative to the root,
	// with a %s placeholder for the version directory.
	archive string
}

// NewServer opens the data root. cacheMB bounds the directory cache.
func NewServer(dataRoot string, cacheMB int, log *slog.Logger) (*Server, error) {
	if cacheMB <= 0 {
		cacheMB = 16
	}
	srv, err := pmtiles.NewServerWithBucket(
		pmtiles.NewFileBucket(dataRoot), "", stdLogger(log), cacheMB, "")
	if err != nil {
		return nil, fmt.Errorf("pmtiles server: %w", err)
	}
	srv.Start()
	return &Server{
		srv:     srv,
		archive: "%s/" + strings.TrimSuffix(dataset.TilesFile, ".pmtiles"),
	}, nil
}

// stdLogger adapts slog for go-pmtiles. The library logs every range read
// ("fetching ... / fetched ...") through the logger; those lines are dropped,
// everything else (failed reads, changed archives) is kept as a warning.
func stdLogger(log *slog.Logger) *stdlog.Logger {
	return stdlog.New(logFilter{log}, "", 0)
}

type logFilter struct{ log *slog.Logger }

func (f logFilter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if strings.HasPrefix(line, "fetching ") || strings.HasPrefix(line, "fetched ") {
		return len(p), nil
	}
	f.log.Warn(line, "component", "pmtiles")
	return len(p), nil
}

// Tile returns the tile z/x/y from the archive of version. Pass
// dataset.CurrentLink for the live version: the "current" symlink is resolved
// on every request, and go-pmtiles notices a changed archive through its ETag
// (mtime+size) and reloads its directory cache, so switching versions needs
// no restart.
func (s *Server) Tile(ctx context.Context, version string, z, x, y int) Result {
	path := "/" + fmt.Sprintf(s.archive, version) + "/" + strconv.Itoa(z) + "/" + strconv.Itoa(x) + "/" + strconv.Itoa(y) + ".mvt"
	status, headers, body := s.srv.Get(ctx, path)
	return Result{Status: status, Headers: headers, Body: body}
}

// Ready reports whether the archive of version can be read (its z0 tile is
// present). It is what /readyz uses.
func (s *Server) Ready(ctx context.Context, version string) error {
	r := s.Tile(ctx, version, 0, 0, 0)
	switch r.Status {
	case 200, 204:
		return nil
	case 404:
		return fmt.Errorf("tile archive for %q not found", version)
	default:
		return fmt.Errorf("tile archive for %q: status %d: %s", version, r.Status, strings.TrimSpace(string(r.Body)))
	}
}

// ValidTile reports whether z/x/y is a possible tile of the archive.
func ValidTile(z, x, y int) bool {
	if z < 0 || z > MaxZoom || x < 0 || y < 0 {
		return false
	}
	n := 1 << z
	return x < n && y < n
}

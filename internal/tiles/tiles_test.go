package tiles

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/protomaps/go-pmtiles/pmtiles"

	"map-service/internal/dataset"
)

type zxy struct{ z, x, y int }

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return b.Bytes()
}

// writeArchive builds a minimal valid PMTiles v3 MVT archive from the given
// (already gzip-compressed) tiles.
func writeArchive(t *testing.T, path string, tiles map[zxy][]byte) {
	t.Helper()
	type item struct {
		id   uint64
		data []byte
	}
	var items []item
	for k, v := range tiles {
		items = append(items, item{pmtiles.ZxyToID(uint8(k.z), uint32(k.x), uint32(k.y)), v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	var entries []pmtiles.EntryV3
	var data []byte
	for _, it := range items {
		entries = append(entries, pmtiles.EntryV3{TileID: it.id, Offset: uint64(len(data)), Length: uint32(len(it.data)), RunLength: 1})
		data = append(data, it.data...)
	}
	root := pmtiles.SerializeEntries(entries, pmtiles.Gzip)
	meta, err := pmtiles.SerializeMetadata(map[string]any{"name": "test"}, pmtiles.Gzip)
	if err != nil {
		t.Fatal(err)
	}
	h := pmtiles.HeaderV3{
		SpecVersion:         3,
		InternalCompression: pmtiles.Gzip,
		TileCompression:     pmtiles.Gzip,
		TileType:            pmtiles.Mvt,
		MaxZoom:             14,
		RootOffset:          pmtiles.HeaderV3LenBytes,
		RootLength:          uint64(len(root)),
	}
	h.MetadataOffset = h.RootOffset + h.RootLength
	h.MetadataLength = uint64(len(meta))
	h.LeafDirectoryOffset = h.MetadataOffset + h.MetadataLength
	h.TileDataOffset = h.LeafDirectoryOffset
	h.TileDataLength = uint64(len(data))
	out := append(pmtiles.SerializeHeader(h), root...)
	out = append(out, meta...)
	out = append(out, data...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newServer(t *testing.T, root string) *Server {
	t.Helper()
	prometheus.DefaultRegisterer = prometheus.NewRegistry() // go-pmtiles registers global metrics
	s, err := NewServer(root, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestTileFromVersionedArchive(t *testing.T) {
	root := t.TempDir()
	const v = "20261014T000000Z"
	writeArchive(t, filepath.Join(root, v, dataset.TilesFile), map[zxy][]byte{
		{0, 0, 0}:         gz(t, "world"),
		{14, 11442, 5475}: gz(t, "astana"),
	})
	s := newServer(t, root)
	ctx := context.Background()

	r := s.Tile(ctx, v, 14, 11442, 5475)
	if r.Status != 200 || gunzip(t, r.Body) != "astana" {
		t.Fatalf("tile: status=%d body=%q", r.Status, r.Body)
	}
	if r.Headers["Content-Encoding"] != "gzip" || r.Headers["ETag"] == "" || r.Headers["Content-Type"] == "" {
		t.Errorf("headers: %v", r.Headers)
	}
	if got := s.Tile(ctx, v, 14, 1, 1); got.Status != 204 {
		t.Errorf("tile absent from the archive: want 204, got %d", got.Status)
	}
	if got := s.Tile(ctx, "20300101T000000Z", 0, 0, 0); got.Status != 404 {
		t.Errorf("missing archive: want 404, got %d", got.Status)
	}
	if err := s.Ready(ctx, v); err != nil {
		t.Errorf("Ready: %v", err)
	}
	if err := s.Ready(ctx, "20300101T000000Z"); err == nil {
		t.Error("Ready must fail for a missing archive")
	}
}

// The live version is read through the "current" symlink; after it is
// repointed the server must serve the new archive without a restart.
func TestSwitchingCurrentServesNewArchive(t *testing.T) {
	root := t.TempDir()
	const v1, v2 = "20261014T000000Z", "20261021T000000Z"
	writeArchive(t, filepath.Join(root, v1, dataset.TilesFile), map[zxy][]byte{{0, 0, 0}: gz(t, "old data")})
	writeArchive(t, filepath.Join(root, v2, dataset.TilesFile), map[zxy][]byte{{0, 0, 0}: gz(t, "brand new data, longer")})
	// Different mtime/size is what changes the ETag go-pmtiles watches.
	_ = os.Chtimes(filepath.Join(root, v2, dataset.TilesFile), time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	if err := dataset.SwitchCurrent(root, v1); err != nil {
		t.Fatal(err)
	}
	s := newServer(t, root)
	ctx := context.Background()
	r1 := s.Tile(ctx, dataset.CurrentLink, 0, 0, 0)
	if r1.Status != 200 || gunzip(t, r1.Body) != "old data" {
		t.Fatalf("before switch: %d %q", r1.Status, r1.Body)
	}
	if err := dataset.SwitchCurrent(root, v2); err != nil {
		t.Fatal(err)
	}
	r2 := s.Tile(ctx, dataset.CurrentLink, 0, 0, 0)
	if r2.Status != 200 || gunzip(t, r2.Body) != "brand new data, longer" {
		t.Fatalf("after switch: %d %q", r2.Status, r2.Body)
	}
	if r1.Headers["ETag"] == r2.Headers["ETag"] {
		t.Error("ETag must change with the data")
	}
}

func TestValidTile(t *testing.T) {
	cases := []struct {
		z, x, y int
		want    bool
	}{
		{0, 0, 0, true}, {14, 16383, 16383, true}, {14, 16384, 0, false}, {1, 2, 0, false},
		{15, 0, 0, false}, {-1, 0, 0, false}, {3, -1, 0, false}, {3, 0, 8, false},
	}
	for _, c := range cases {
		if got := ValidTile(c.z, c.x, c.y); got != c.want {
			t.Errorf("ValidTile(%d,%d,%d) = %v, want %v", c.z, c.x, c.y, got, c.want)
		}
	}
}

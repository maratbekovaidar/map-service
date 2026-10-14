// Package dataset manages the on-disk layout of data versions:
//
//	data/
//	  current -> 20261014T031500Z      (relative symlink, the live version)
//	  20261014T031500Z/
//	    city.osm.pbf
//	    manifest.json                  (present only when the build is complete)
//	    osrm/<profile>/city.osrm*
//	    tiles/city.pmtiles
//	    photon/photon_data/
//	  cache/                           (downloaded country extract)
//	  logs/
//
// The gateway uses it to learn the live version, the updater to build, switch
// and prune versions.
package dataset

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	// CurrentLink is the name of the symlink that points at the live version.
	CurrentLink = "current"
	// ManifestFile marks a version as completely built.
	ManifestFile = "manifest.json"
	// PBFFile is the city extract all services are built from.
	PBFFile = "city.osm.pbf"
	// TilesFile is the PMTiles archive, relative to a version directory.
	TilesFile = "tiles/city.pmtiles"

	versionLayout = "20060102T150405Z"
)

var versionRe = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

// ErrNoCurrent means no version has been switched live yet.
var ErrNoCurrent = errors.New("dataset: no current version")

// NewVersion returns the version name for a build started at t (UTC).
func NewVersion(t time.Time) string { return t.UTC().Format(versionLayout) }

// ValidVersion reports whether s looks like a version name.
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

// Dir returns the directory of a version.
func Dir(dataDir, version string) string { return filepath.Join(dataDir, version) }

// Current returns the live version by resolving the "current" symlink.
func Current(dataDir string) (string, error) {
	target, err := os.Readlink(filepath.Join(dataDir, CurrentLink))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNoCurrent
		}
		return "", fmt.Errorf("dataset: read current link: %w", err)
	}
	v := filepath.Base(target)
	if !ValidVersion(v) {
		return "", fmt.Errorf("dataset: current link points at %q, not a version", target)
	}
	return v, nil
}

// SwitchCurrent atomically repoints "current" at version. The link is
// relative, so it resolves identically on the host and inside containers that
// mount the data directory elsewhere.
func SwitchCurrent(dataDir, version string) error {
	if !ValidVersion(version) {
		return fmt.Errorf("dataset: invalid version %q", version)
	}
	if _, err := os.Stat(filepath.Join(dataDir, version)); err != nil {
		return fmt.Errorf("dataset: version dir: %w", err)
	}
	tmp := filepath.Join(dataDir, CurrentLink+".tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		return fmt.Errorf("dataset: create link: %w", err)
	}
	// rename(2) over an existing symlink is atomic: readers see the old or
	// the new target, never a missing link.
	if err := os.Rename(tmp, filepath.Join(dataDir, CurrentLink)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dataset: switch link: %w", err)
	}
	return nil
}

// Versions lists version directories (complete or not), oldest first.
func Versions(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && ValidVersion(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out) // the layout sorts chronologically
	return out, nil
}

// Complete reports whether a version carries a manifest.
func Complete(dataDir, version string) bool {
	_, err := os.Stat(filepath.Join(dataDir, version, ManifestFile))
	return err == nil
}

// Manifest describes a built version.
type Manifest struct {
	Version     string    `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	City        string    `json:"city"`
	BBox        string    `json:"bbox"`
	ExtractMode string    `json:"extract_mode"`
	Source      Source    `json:"source"`
	Profiles    []string  `json:"profiles"`
	Languages   []string  `json:"languages"`
	Steps       []Step    `json:"steps"`
}

// Source describes where the country extract came from.
type Source struct {
	URL          string `json:"url"`
	SizeBytes    int64  `json:"size_bytes"`
	OSMTimestamp string `json:"osm_timestamp,omitempty"`
}

// Step records the duration of one pipeline step.
type Step struct {
	Name      string  `json:"name"`
	DurationS float64 `json:"duration_s"`
}

// WriteManifest writes the manifest atomically (temp file + rename).
func WriteManifest(dataDir string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(dataDir, m.Version)
	tmp := filepath.Join(dir, ManifestFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ManifestFile))
}

// ReadManifest reads the manifest of a version.
func ReadManifest(dataDir, version string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dataDir, version, ManifestFile))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

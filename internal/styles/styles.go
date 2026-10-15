// Package styles is the gateway's catalogue of map styles: the style
// documents installed as STATIC_DIR/styles/<id>.json and the default one that
// /tiles/style.json serves. An admin can switch the default at runtime
// (PUT /v1/styles/default); the choice is written to STATE_DIR so it survives
// restarts, and DEFAULT_STYLE applies until a choice is made.
package styles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrUnknown is returned for a style id that is not installed.
var ErrUnknown = errors.New("styles: no such style")

// stateFileName holds the chosen default style id inside STATE_DIR.
const stateFileName = "default-style"

// ValidID reports whether id can name a style: it is a file name and the
// {id} of /tiles/styles/{id}.json, so only [a-z0-9-] is allowed.
func ValidID(id string) bool {
	if id == "" || len(id) > 48 || id[0] == '-' {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Info describes an installed style.
type Info struct {
	ID          string
	Name        string
	Description string
}

// Catalog lists the installed styles and keeps the default one.
type Catalog struct {
	dir       string // <id>.json files
	stateFile string // "" = the choice is kept in memory only
	fallback  string // DEFAULT_STYLE

	mu     sync.RWMutex
	chosen string // set by SetDefault or loaded from stateFile
}

// New returns a catalogue of the styles in dir. stateDir may be empty.
func New(dir, stateDir, fallback string) *Catalog {
	c := &Catalog{dir: dir, fallback: fallback}
	if stateDir != "" {
		c.stateFile = filepath.Join(stateDir, stateFileName)
	}
	return c
}

// Load reads a previously chosen default from the state directory. A missing
// file is not an error; an id that is not installed is ignored (Default falls
// back to DEFAULT_STYLE), so a removed style cannot break /tiles/style.json.
func (c *Catalog) Load() error {
	if c.stateFile == "" {
		return nil
	}
	b, err := os.ReadFile(c.stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("styles: read %s: %w", c.stateFile, err)
	}
	id := strings.TrimSpace(string(b))
	if !ValidID(id) {
		return fmt.Errorf("styles: %s holds an invalid style id %q", c.stateFile, id)
	}
	c.mu.Lock()
	c.chosen = id
	c.mu.Unlock()
	return nil
}

func (c *Catalog) path(id string) string { return filepath.Join(c.dir, id+".json") }

func (c *Catalog) installed(id string) bool {
	if !ValidID(id) {
		return false
	}
	st, err := os.Stat(c.path(id))
	return err == nil && st.Mode().IsRegular()
}

// Default returns the id of the default style: the admin's choice when it is
// installed, else DEFAULT_STYLE. ErrUnknown means neither is installed.
func (c *Catalog) Default() (string, error) {
	c.mu.RLock()
	chosen := c.chosen
	c.mu.RUnlock()
	for _, id := range []string{chosen, c.fallback} {
		if id != "" && c.installed(id) {
			return id, nil
		}
	}
	return "", ErrUnknown
}

// SetDefault makes id the default style and persists the choice.
func (c *Catalog) SetDefault(id string) error {
	if !c.installed(id) {
		return ErrUnknown
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stateFile != "" {
		if err := writeAtomic(c.stateFile, []byte(id+"\n")); err != nil {
			return fmt.Errorf("styles: persist the default style: %w", err)
		}
	}
	c.chosen = id
	return nil
}

// Persistent reports whether SetDefault survives a restart.
func (c *Catalog) Persistent() bool { return c.stateFile != "" }

// Read returns the style document id. ErrUnknown if it is not installed.
func (c *Catalog) Read(id string) ([]byte, error) {
	if !ValidID(id) {
		return nil, ErrUnknown
	}
	b, err := os.ReadFile(c.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrUnknown
	}
	return b, err
}

// List returns the installed styles sorted by id. Name and description come
// from the style document ("name", metadata "map-service:description").
func (c *Catalog) List() ([]Info, error) {
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidID(id) || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(c.path(id))
		if err != nil {
			return nil, err
		}
		var doc struct {
			Name     string `json:"name"`
			Metadata struct {
				Description string `json:"map-service:description"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("styles: %s: %w", e.Name(), err)
		}
		name := doc.Name
		if name == "" {
			name = id
		}
		out = append(out, Info{ID: id, Name: name, Description: doc.Metadata.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// writeAtomic replaces path with data via a temporary file in the same
// directory, so a crash never leaves a truncated file behind.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

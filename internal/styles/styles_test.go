package styles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func install(t *testing.T, dir string, docs map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range docs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fixture(t *testing.T) (dir, state string) {
	t.Helper()
	root := t.TempDir()
	dir, state = filepath.Join(root, "styles"), filepath.Join(root, "state")
	install(t, dir, map[string]string{
		"liberty.json":  `{"name":"OSM Liberty","metadata":{"map-service:description":"classic"},"layers":[]}`,
		"warm.json":     `{"name":"Warm","metadata":{"map-service:description":"warm"},"layers":[]}`,
		"bare.json":     `{"layers":[]}`,
		"Bad Name.json": `{}`,
		"notes.txt":     `not a style`,
	})
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, state
}

func TestList(t *testing.T) {
	dir, _ := fixture(t)
	list, err := New(dir, "", "liberty").List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, st := range list {
		got = append(got, st.ID+"|"+st.Name+"|"+st.Description)
	}
	if want := "bare|bare|,liberty|OSM Liberty|classic,warm|Warm|warm"; strings.Join(got, ",") != want {
		t.Errorf("list = %v, want %s", got, want)
	}
	if list, err := New(filepath.Join(dir, "missing"), "", "liberty").List(); err != nil || len(list) != 0 {
		t.Errorf("a missing directory is an empty catalogue: %v %v", list, err)
	}
	install(t, dir, map[string]string{"broken.json": `{`})
	if _, err := New(dir, "", "liberty").List(); err == nil {
		t.Error("a broken style document must be reported")
	}
}

func TestDefaultFallsBackAndPersists(t *testing.T) {
	dir, state := fixture(t)
	c := New(dir, state, "liberty")
	if err := c.Load(); err != nil {
		t.Fatal(err)
	}
	if id, err := c.Default(); err != nil || id != "liberty" {
		t.Fatalf("DEFAULT_STYLE applies before a choice: %q %v", id, err)
	}
	if err := c.SetDefault("nope"); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown style: %v", err)
	}
	if err := c.SetDefault("../liberty"); !errors.Is(err, ErrUnknown) {
		t.Errorf("path-like id: %v", err)
	}
	if err := c.SetDefault("warm"); err != nil {
		t.Fatal(err)
	}
	if id, _ := c.Default(); id != "warm" {
		t.Errorf("after SetDefault: %q", id)
	}
	if !c.Persistent() {
		t.Error("with a state dir the choice is persistent")
	}

	// A restart restores the choice.
	c2 := New(dir, state, "liberty")
	if err := c2.Load(); err != nil {
		t.Fatal(err)
	}
	if id, _ := c2.Default(); id != "warm" {
		t.Errorf("after restart: %q", id)
	}
	entries, _ := os.ReadDir(state)
	if len(entries) != 1 || entries[0].Name() != stateFileName {
		t.Errorf("state dir must hold only the state file (no temp files): %v", entries)
	}

	// The chosen style disappears (e.g. a new image without it): back to DEFAULT_STYLE.
	if err := os.Remove(filepath.Join(dir, "warm.json")); err != nil {
		t.Fatal(err)
	}
	if id, _ := c2.Default(); id != "liberty" {
		t.Errorf("removed choice must fall back: %q", id)
	}
	// Neither is installed.
	if _, err := New(dir, "", "missing").Default(); !errors.Is(err, ErrUnknown) {
		t.Errorf("no default installed: %v", err)
	}
}

func TestMemoryOnlyAndBadState(t *testing.T) {
	dir, state := fixture(t)
	c := New(dir, "", "liberty")
	if err := c.Load(); err != nil || c.Persistent() {
		t.Fatalf("memory-only catalogue: %v %v", err, c.Persistent())
	}
	if err := c.SetDefault("bare"); err != nil {
		t.Fatal(err)
	}
	if id, _ := c.Default(); id != "bare" {
		t.Errorf("memory choice: %q", id)
	}

	if err := os.WriteFile(filepath.Join(state, stateFileName), []byte("../../etc/passwd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = New(dir, state, "liberty")
	if err := c.Load(); err == nil {
		t.Error("an invalid saved id must be reported")
	}
	if id, _ := c.Default(); id != "liberty" {
		t.Errorf("an invalid saved id must be ignored: %q", id)
	}
}

func TestRead(t *testing.T) {
	dir, _ := fixture(t)
	c := New(dir, "", "liberty")
	if b, err := c.Read("liberty"); err != nil || !strings.Contains(string(b), "OSM Liberty") {
		t.Errorf("read: %s %v", b, err)
	}
	for _, id := range []string{"missing", "../styles/liberty", "Bad Name", ""} {
		if _, err := c.Read(id); !errors.Is(err, ErrUnknown) {
			t.Errorf("Read(%q) = %v, want ErrUnknown", id, err)
		}
	}
}

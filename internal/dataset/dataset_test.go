package dataset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVersionNames(t *testing.T) {
	v := NewVersion(time.Date(2026, 10, 14, 3, 15, 9, 0, time.FixedZone("x", 5*3600)))
	if v != "20261013T221509Z" { // converted to UTC
		t.Fatalf("NewVersion = %q", v)
	}
	if !ValidVersion(v) {
		t.Errorf("%q should be valid", v)
	}
	for _, bad := range []string{"", "current", "2026-10-14", "20261013T221509", "../20261013T221509Z"} {
		if ValidVersion(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSwitchCurrentAndCurrent(t *testing.T) {
	dir := t.TempDir()
	if _, err := Current(dir); !errors.Is(err, ErrNoCurrent) {
		t.Fatalf("Current on empty dir: %v", err)
	}
	v1, v2 := "20260101T000000Z", "20260102T000000Z"
	for _, v := range []string{v1, v2} {
		if err := os.Mkdir(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := SwitchCurrent(dir, v1); err != nil {
		t.Fatal(err)
	}
	if got, _ := Current(dir); got != v1 {
		t.Fatalf("Current = %q, want %q", got, v1)
	}
	// The link must be relative so it resolves inside containers too.
	target, _ := os.Readlink(filepath.Join(dir, CurrentLink))
	if filepath.IsAbs(target) {
		t.Errorf("link target %q must be relative", target)
	}
	if err := SwitchCurrent(dir, v2); err != nil {
		t.Fatal(err)
	}
	if got, _ := Current(dir); got != v2 {
		t.Fatalf("Current after switch = %q, want %q", got, v2)
	}
	if err := SwitchCurrent(dir, "20300101T000000Z"); err == nil {
		t.Error("switching to a missing version must fail")
	}
	if err := SwitchCurrent(dir, "bogus"); err == nil {
		t.Error("switching to an invalid version must fail")
	}
	// A failed switch leaves the old link intact.
	if got, _ := Current(dir); got != v2 {
		t.Errorf("Current after failed switch = %q, want %q", got, v2)
	}
}

func TestVersionsAndManifest(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"20260103T000000Z", "20260101T000000Z", "cache", "logs"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "20260102T000000Z"), nil, 0o644); err != nil { // a file, not a dir
		t.Fatal(err)
	}
	got, err := Versions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "20260101T000000Z" || got[1] != "20260103T000000Z" {
		t.Fatalf("Versions = %v", got)
	}
	if Complete(dir, got[0]) {
		t.Error("version without manifest must not be complete")
	}
	m := Manifest{Version: got[0], CreatedAt: time.Unix(1, 0).UTC(), City: "Astana", Profiles: []string{"car"}}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	if !Complete(dir, got[0]) {
		t.Error("version with manifest must be complete")
	}
	back, err := ReadManifest(dir, got[0])
	if err != nil || back.City != "Astana" || back.Profiles[0] != "car" {
		t.Fatalf("ReadManifest = %+v, %v", back, err)
	}
	if v, err := Versions(filepath.Join(dir, "missing")); err != nil || v != nil {
		t.Errorf("Versions(missing) = %v, %v", v, err)
	}
}

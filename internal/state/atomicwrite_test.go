package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicWriteFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.json")

	if err := atomicWriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "v1" {
		t.Fatalf("read after first write = %q, %v; want v1", got, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}

	// Overwrite with new content + perms: rename replaces in place and re-applies perm.
	if err := atomicWriteFile(path, []byte("v2-longer"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "v2-longer" {
		t.Fatalf("read after overwrite = %q, %v; want v2-longer", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("perm after overwrite = %o, want 644", info.Mode().Perm())
	}

	// No temp files are left behind in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1 (just the target)", len(entries))
	}
}

// TestSaveStateAtomicNoTornReads asserts repeated Save/Load round-trips through
// the atomic writer keep state.json well-formed and complete (the property the
// backup loop relies on when it reads the file unlocked).
func TestSaveStateAtomicNoTornReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for i := range 5 {
		if err := Save(&State{}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		// An unlocked read (as the backup loop does) must always see a complete file.
		data, err := os.ReadFile(StatePath())
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(data) == 0 || data[len(data)-1] != '}' {
			t.Fatalf("state.json looks truncated on iter %d: %q", i, data)
		}
	}
	// The atomic writer must not leave temp files in ~/.fleet.
	entries, _ := os.ReadDir(FleetDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file in fleet dir: %s", e.Name())
		}
	}
}

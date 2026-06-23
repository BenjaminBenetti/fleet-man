package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.json")

	if err := Write(path, []byte("v1"), 0o600); err != nil {
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
	if err := Write(path, []byte("v2-longer"), 0o644); err != nil {
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

func TestWriteMissingParentDirErrors(t *testing.T) {
	// The parent dir must already exist; a missing one surfaces as an error
	// rather than silently creating it.
	path := filepath.Join(t.TempDir(), "nope", "thing.json")
	if err := Write(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error writing into a missing parent dir, got nil")
	}
}

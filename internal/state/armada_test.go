package state

import (
	"os"
	"testing"
)

// TestArmadaRoundTrip saves a registry and loads it back, verifying the
// content survives and the file is written 0600 (it carries bearer tokens —
// stricter than config.json's 0644).
func TestArmadaRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	in := &Armada{Remotes: []ArmadaRemote{
		{URL: "https://gw.example.com/abc", Token: "tok-1"},
		{URL: "http://gw2.example.com:8080/def", Token: "tok-2"},
	}}
	if err := SaveArmada(in); err != nil {
		t.Fatalf("SaveArmada: %v", err)
	}

	info, err := os.Stat(ArmadaPath())
	if err != nil {
		t.Fatalf("stat armada file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("armada file mode = %o, want 0600", perm)
	}

	out, err := LoadArmada()
	if err != nil {
		t.Fatalf("LoadArmada: %v", err)
	}
	if len(out.Remotes) != 2 {
		t.Fatalf("loaded %d remotes, want 2", len(out.Remotes))
	}
	if out.Remotes[0] != in.Remotes[0] || out.Remotes[1] != in.Remotes[1] {
		t.Fatalf("round trip mismatch: %+v", out.Remotes)
	}
}

// TestLoadArmadaMissingFileReturnsEmpty verifies a never-written registry
// loads as empty, not as an error.
func TestLoadArmadaMissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a, err := LoadArmada()
	if err != nil {
		t.Fatalf("LoadArmada: %v", err)
	}
	if len(a.Remotes) != 0 {
		t.Fatalf("expected empty registry, got %+v", a.Remotes)
	}
}

// TestSaveArmadaNormalizes verifies entries are trimmed and URL-less entries
// (which identify nothing) are dropped on save.
func TestSaveArmadaNormalizes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	in := &Armada{Remotes: []ArmadaRemote{
		{URL: "  https://gw.example.com/abc  ", Token: "  tok  "},
		{URL: "   ", Token: "orphan-token"},
	}}
	if err := SaveArmada(in); err != nil {
		t.Fatalf("SaveArmada: %v", err)
	}

	out, err := LoadArmada()
	if err != nil {
		t.Fatalf("LoadArmada: %v", err)
	}
	if len(out.Remotes) != 1 {
		t.Fatalf("loaded %d remotes, want 1 (empty-URL entry dropped)", len(out.Remotes))
	}
	if out.Remotes[0].URL != "https://gw.example.com/abc" || out.Remotes[0].Token != "tok" {
		t.Fatalf("entry not trimmed: %+v", out.Remotes[0])
	}
}

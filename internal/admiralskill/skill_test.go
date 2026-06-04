package admiralskill

import (
	"os"
	"path/filepath"
	"testing"
)

// withHome points HOME (and USERPROFILE on Windows) at a temp dir so
// EnsureInstalled writes into an isolated ~/.claude and never touches the real
// user home.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestEnsureInstalledWritesSkillAndHash verifies a clean install writes the
// embedded SKILL.md and a matching .hash into ~/.claude/skills/fleet-admiral.
func TestEnsureInstalledWritesSkillAndHash(t *testing.T) {
	home := withHome(t)

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() = %v, want nil", err)
	}

	dir := filepath.Join(home, ".claude", "skills", skillName)

	got, err := os.ReadFile(filepath.Join(dir, skillFile))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if string(got) != string(skillContent) {
		t.Errorf("installed SKILL.md does not match embedded content")
	}

	hash, err := os.ReadFile(filepath.Join(dir, hashFile))
	if err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if string(hash) != contentHash(skillContent) {
		t.Errorf("hash file = %q, want %q", hash, contentHash(skillContent))
	}
}

// TestEnsureInstalledIsIdempotent verifies the fast path: when the hash already
// matches, EnsureInstalled does not rewrite SKILL.md.
func TestEnsureInstalledIsIdempotent(t *testing.T) {
	home := withHome(t)

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("first EnsureInstalled() = %v", err)
	}

	skillPath := filepath.Join(home, ".claude", "skills", skillName, skillFile)
	info, err := os.Stat(skillPath)
	if err != nil {
		t.Fatalf("stat SKILL.md: %v", err)
	}

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("second EnsureInstalled() = %v", err)
	}

	info2, err := os.Stat(skillPath)
	if err != nil {
		t.Fatalf("stat SKILL.md after second run: %v", err)
	}
	if !info2.ModTime().Equal(info.ModTime()) {
		t.Errorf("SKILL.md was rewritten on the no-op path (mtime changed)")
	}
}

// TestEnsureInstalledRewritesOnHashMismatch verifies a stale hash forces a
// rewrite of SKILL.md back to the embedded content.
func TestEnsureInstalledRewritesOnHashMismatch(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".claude", "skills", skillName)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-seed a stale install: wrong content + a non-matching hash.
	if err := os.WriteFile(filepath.Join(dir, skillFile), []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hashFile), []byte("deadbeef"), 0o644); err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	if err := EnsureInstalled(); err != nil {
		t.Fatalf("EnsureInstalled() = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, skillFile))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if string(got) != string(skillContent) {
		t.Errorf("SKILL.md was not refreshed to embedded content on hash mismatch")
	}
	hash, _ := os.ReadFile(filepath.Join(dir, hashFile))
	if string(hash) != contentHash(skillContent) {
		t.Errorf("hash not updated after rewrite")
	}
}

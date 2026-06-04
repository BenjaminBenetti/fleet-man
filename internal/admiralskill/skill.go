// Package admiralskill installs the "Fleet Admiral" Claude Code skill into the
// user's personal skills directory (~/.claude/skills/fleet-admiral) on client
// startup.
//
// The skill is a single SKILL.md that teaches a coding agent to act as an
// orchestrator: spinning up fleet instances and delegating work to the agents
// inside them via the `fleet` CLI. It is embedded into the fleet binary at build
// time (//go:embed) so deployment is just the binary — there is no separate
// asset to ship.
//
// Installation is hash-gated to keep startup cheap: a .hash file alongside the
// installed SKILL.md records the sha256 of the content that was written. On each
// startup we compare the embedded content's hash to that file and only rewrite
// when they differ (a fresh install, or a fleet upgrade that changed the skill).
// The common case — already installed, unchanged — costs one small file read.
package admiralskill

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// ===========================================
// Constants
// ===========================================

// skillName is both the skill's frontmatter name and its install directory
// basename under ~/.claude/skills.
const skillName = "fleet-admiral"

// skillFile is the canonical Claude Code skill manifest filename.
const skillFile = "SKILL.md"

// hashFile records the sha256 of the SKILL.md last written here. It lives inside
// the skill folder (dotfile so it doesn't read as skill content) and is the
// fast-path gate that lets startup skip a rewrite when nothing changed.
const hashFile = ".hash"

// ===========================================
// Embedded content
// ===========================================

// skillContent is the SKILL.md packed into the binary at build time.
//
//go:embed SKILL.md
var skillContent []byte

// ===========================================
// Public API
// ===========================================

// EnsureInstalled writes the Fleet Admiral skill into ~/.claude/skills if it is
// missing or out of date, and is a cheap no-op otherwise.
//
// It is best-effort: every failure is logged and returned, never propagated as a
// fatal startup error — a skill-install hiccup must never stop `fleet` from
// running. Callers on the startup path should log the error (if any) and carry
// on.
func EnsureInstalled() error {
	dir, err := skillDir()
	if err != nil {
		flog.Warn("admiralskill: cannot resolve skills dir", "err", err)
		return err
	}

	want := contentHash(skillContent)

	// Fast path: already installed with the exact content we'd write.
	if got, err := os.ReadFile(filepath.Join(dir, hashFile)); err == nil && string(got) == want {
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		flog.Warn("admiralskill: mkdir failed", "dir", dir, "err", err)
		return err
	}

	// Write the skill first, then the hash. If the process dies between the two,
	// the hash is absent/stale and we simply rewrite next startup — the SKILL.md
	// is never recorded as current until it is actually on disk.
	if err := os.WriteFile(filepath.Join(dir, skillFile), skillContent, 0o644); err != nil {
		flog.Warn("admiralskill: write SKILL.md failed", "dir", dir, "err", err)
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, hashFile), []byte(want), 0o644); err != nil {
		flog.Warn("admiralskill: write hash failed", "dir", dir, "err", err)
		return err
	}

	flog.Info("admiralskill: installed Fleet Admiral skill", "dir", dir)
	return nil
}

// ===========================================
// Internal helpers
// ===========================================

// skillDir is ~/.claude/skills/fleet-admiral, the personal-skills location
// Claude Code loads on startup.
func skillDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills", skillName), nil
}

// contentHash returns the hex sha256 of b, used as the .hash file contents.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

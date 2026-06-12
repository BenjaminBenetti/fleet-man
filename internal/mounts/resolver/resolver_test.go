package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestResolveReturnsEmptyWhenNoSettingsEnabled verifies that an empty
// FleetSettings produces no mounts, no symlinks, and no host-side
// side effects.
func TestResolveReturnsEmptyWhenNoSettingsEnabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	resolved, err := Resolve("my-fleet", fleet.FleetSettings{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolved.Mounts) != 0 || len(resolved.Symlinks) != 0 {
		t.Fatalf("Resolve() = %+v, want empty", resolved)
	}
}

// TestResolveCreatesHostDirsAndReturnsMounts checks that an enabled
// directory setting creates the host dir and returns a mount entry.
// With ClaudeCodeMount also enabled, the shared files mount + the
// .claude.json symlink ride along.
func TestResolveCreatesHostDirsAndReturnsMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("alpha", fleet.FleetSettings{ClaudeCodeMount: true, CodexMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	wantClaudeHost := filepath.Join(home, ".fleet", "workspaces", "alpha", ".claude")
	wantCodexHost := filepath.Join(home, ".fleet", "workspaces", "alpha", ".codex")
	wantSharedHost := filepath.Join(home, ".fleet", "workspaces", "alpha", "files")

	byContainerPath := map[string]string{}
	for _, mount := range resolved.Mounts {
		byContainerPath[mount.ContainerPath] = mount.LocalPath
	}

	if got := byContainerPath["/home/vscode/.claude"]; got != wantClaudeHost {
		t.Errorf("claude host path = %q, want %q", got, wantClaudeHost)
	}
	if got := byContainerPath["/home/vscode/.codex"]; got != wantCodexHost {
		t.Errorf("codex host path = %q, want %q", got, wantCodexHost)
	}
	if got := byContainerPath["/fleet-mounts/files"]; got != wantSharedHost {
		t.Errorf("shared files host path = %q, want %q", got, wantSharedHost)
	}

	for _, hostPath := range []string{wantClaudeHost, wantCodexHost, wantSharedHost} {
		info, err := os.Stat(hostPath)
		if err != nil {
			t.Errorf("expected host dir %s to exist: %v", hostPath, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", hostPath)
		}
	}
}

// TestResolveCreatesClaudeJSONSymlinkAndHostFile verifies the
// single-file mount strategy: enabling ClaudeCodeMount produces a
// Symlink pointing the container's ~/.claude.json at the file inside
// the shared mount, and creates the host file empty so the bind
// mount target is valid.
func TestResolveCreatesClaudeJSONSymlinkAndHostFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("alpha", fleet.FleetSettings{ClaudeCodeMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.Symlinks) != 1 {
		t.Fatalf("len(Symlinks) = %d, want 1: %+v", len(resolved.Symlinks), resolved.Symlinks)
	}
	link := resolved.Symlinks[0]
	if link.Target != "/home/vscode/.claude.json" {
		t.Errorf("Symlink.Target = %q, want %q", link.Target, "/home/vscode/.claude.json")
	}
	if link.Source != "/fleet-mounts/files/.claude.json" {
		t.Errorf("Symlink.Source = %q, want %q", link.Source, "/fleet-mounts/files/.claude.json")
	}

	hostFile := filepath.Join(home, ".fleet", "workspaces", "alpha", "files", ".claude.json")
	info, err := os.Stat(hostFile)
	if err != nil {
		t.Fatalf("expected host file %s to exist: %v", hostFile, err)
	}
	if info.IsDir() {
		t.Errorf("%s is a directory, want regular file", hostFile)
	}
}

// TestResolveClaudeJSONSymlinkCarriesSeedContent verifies the
// Claude-specific seed: an empty ~/.claude.json crashes the CLI on
// install, so the symlink for it must carry a "{}" seed that the
// post-Up script writes once nothing else has populated the file.
func TestResolveClaudeJSONSymlinkCarriesSeedContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	resolved, err := Resolve("alpha", fleet.FleetSettings{ClaudeCodeMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolved.Symlinks) != 1 {
		t.Fatalf("len(Symlinks) = %d, want 1", len(resolved.Symlinks))
	}
	if got := resolved.Symlinks[0].SeedContent; got != "{}" {
		t.Errorf("Symlink.SeedContent = %q, want %q", got, "{}")
	}
}

// TestResolvePreservesExistingHostFile makes sure a second Resolve
// call leaves an already-populated host file alone — important
// because we use the file's contents as the persisted state across
// instance churn.
func TestResolvePreservesExistingHostFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := Resolve("alpha", fleet.FleetSettings{ClaudeCodeMount: true}); err != nil {
		t.Fatalf("first Resolve() error = %v", err)
	}
	hostFile := filepath.Join(home, ".fleet", "workspaces", "alpha", "files", ".claude.json")
	if err := os.WriteFile(hostFile, []byte(`{"loggedIn":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve("alpha", fleet.FleetSettings{ClaudeCodeMount: true}); err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}

	got, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"loggedIn":true}` {
		t.Fatalf("host file overwritten: %q", got)
	}
}

// TestResolveUsesHomeDirSetting verifies that an explicit HomeDir
// reroutes the container side of every directory mount and symlink.
func TestResolveUsesHomeDirSetting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	resolved, err := Resolve("alpha", fleet.FleetSettings{
		ClaudeCodeMount: true,
		HomeDir:         "/root",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	var sawClaudeDir bool
	for _, mount := range resolved.Mounts {
		if mount.ContainerPath == "/root/.claude" {
			sawClaudeDir = true
		}
	}
	if !sawClaudeDir {
		t.Errorf("expected /root/.claude mount, got %+v", resolved.Mounts)
	}

	if len(resolved.Symlinks) != 1 || resolved.Symlinks[0].Target != "/root/.claude.json" {
		t.Errorf("expected symlink at /root/.claude.json, got %+v", resolved.Symlinks)
	}
}

// TestResolveCreatesGhMount verifies that enabling GhMount produces a
// directory mount at <containerHome>/.config/gh backed by a host dir
// under the fleet's mount root, and that no symlinks are involved
// (gh's config dir is self-contained).
func TestResolveCreatesGhMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("gamma", fleet.FleetSettings{GhMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.Symlinks) != 0 {
		t.Errorf("len(Symlinks) = %d, want 0", len(resolved.Symlinks))
	}
	if len(resolved.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1", len(resolved.Mounts))
	}

	wantHost := filepath.Join(home, ".fleet", "workspaces", "gamma", ".config", "gh")
	mount := resolved.Mounts[0]
	if mount.LocalPath != wantHost {
		t.Errorf("LocalPath = %q, want %q", mount.LocalPath, wantHost)
	}
	if mount.ContainerPath != "/home/vscode/.config/gh" {
		t.Errorf("ContainerPath = %q, want %q", mount.ContainerPath, "/home/vscode/.config/gh")
	}

	info, err := os.Stat(wantHost)
	if err != nil {
		t.Fatalf("expected host dir %s to exist: %v", wantHost, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", wantHost)
	}
}

// TestResolveCreatesAuggieMount verifies that enabling AuggieMount produces
// a directory mount at <containerHome>/.augment backed by a host dir under
// the fleet's mount root, and that no symlinks are involved (Auggie's state
// dir — session.json + settings.json — is self-contained).
func TestResolveCreatesAuggieMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("delta", fleet.FleetSettings{AuggieMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.Symlinks) != 0 {
		t.Errorf("len(Symlinks) = %d, want 0", len(resolved.Symlinks))
	}
	if len(resolved.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1", len(resolved.Mounts))
	}

	wantHost := filepath.Join(home, ".fleet", "workspaces", "delta", ".augment")
	mount := resolved.Mounts[0]
	if mount.LocalPath != wantHost {
		t.Errorf("LocalPath = %q, want %q", mount.LocalPath, wantHost)
	}
	if mount.ContainerPath != "/home/vscode/.augment" {
		t.Errorf("ContainerPath = %q, want %q", mount.ContainerPath, "/home/vscode/.augment")
	}

	info, err := os.Stat(wantHost)
	if err != nil {
		t.Fatalf("expected host dir %s to exist: %v", wantHost, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", wantHost)
	}
}

// TestResolveCreatesCustomMounts verifies that each custom mount produces a
// directory mount whose container path is the user-supplied path and whose host
// path lives under the fleet's .mnt directory, with the host dir created.
func TestResolveCreatesCustomMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("delta", fleet.FleetSettings{
		CustomMounts: []string{"/opt/data", "/var/cache/shared"},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.Symlinks) != 0 {
		t.Errorf("len(Symlinks) = %d, want 0", len(resolved.Symlinks))
	}

	byContainerPath := map[string]string{}
	for _, mount := range resolved.Mounts {
		byContainerPath[mount.ContainerPath] = mount.LocalPath
	}

	wantData := filepath.Join(home, ".fleet", "workspaces", "delta", ".mnt", "opt", "data")
	wantCache := filepath.Join(home, ".fleet", "workspaces", "delta", ".mnt", "var", "cache", "shared")

	if got := byContainerPath["/opt/data"]; got != wantData {
		t.Errorf("/opt/data host path = %q, want %q", got, wantData)
	}
	if got := byContainerPath["/var/cache/shared"]; got != wantCache {
		t.Errorf("/var/cache/shared host path = %q, want %q", got, wantCache)
	}

	for _, hostPath := range []string{wantData, wantCache} {
		info, err := os.Stat(hostPath)
		if err != nil {
			t.Errorf("expected host dir %s to exist: %v", hostPath, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", hostPath)
		}
	}
}

// TestResolveCustomMountWinsCollision verifies that when a custom mount's
// container path collides with a managed mount target, the resolver emits
// exactly ONE mount for that path (no "Duplicate mount point" at provision
// time) and it is the custom mount — the documented last-wins behavior.
func TestResolveCustomMountWinsCollision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("epsilon", fleet.FleetSettings{
		ClaudeCodeMount: true,
		// Deliberately collide with the managed Claude mount target.
		CustomMounts: []string{"/home/vscode/.claude"},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	var matches []string
	for _, mount := range resolved.Mounts {
		if mount.ContainerPath == "/home/vscode/.claude" {
			matches = append(matches, mount.LocalPath)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 mount for /home/vscode/.claude (last-wins dedup), got %d: %v", len(matches), matches)
	}
	wantCustomHost := filepath.Join(home, ".fleet", "workspaces", "epsilon", ".mnt", "home", "vscode", ".claude")
	if matches[0] != wantCustomHost {
		t.Errorf("colliding mount host = %q, want the custom mount %q", matches[0], wantCustomHost)
	}
}

// TestResolveSkipsInvalidCustomMounts verifies the resolver defensively skips a
// traversal entry that somehow reached state.json (e.g. hand-edited), never
// building a host path that escapes the fleet's .mnt directory.
func TestResolveSkipsInvalidCustomMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("zeta", fleet.FleetSettings{
		CustomMounts: []string{"/opt/data", "../../escape", "relative"},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1 (only the valid entry): %+v", len(resolved.Mounts), resolved.Mounts)
	}
	if resolved.Mounts[0].ContainerPath != "/opt/data" {
		t.Errorf("ContainerPath = %q, want /opt/data", resolved.Mounts[0].ContainerPath)
	}
}

// TestResolveOnlyEnabledMountsAreReturned ensures disabled toggles do
// not produce mounts, symlinks, or host-side artefacts.
func TestResolveOnlyEnabledMountsAreReturned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	resolved, err := Resolve("beta", fleet.FleetSettings{CodexMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// Codex is directory-only, so no symlinks or shared-files mount.
	if len(resolved.Symlinks) != 0 {
		t.Errorf("len(Symlinks) = %d, want 0", len(resolved.Symlinks))
	}
	if len(resolved.Mounts) != 1 {
		t.Errorf("len(Mounts) = %d, want 1", len(resolved.Mounts))
	}

	claudeHost := filepath.Join(home, ".fleet", "workspaces", "beta", ".claude")
	if _, err := os.Stat(claudeHost); !os.IsNotExist(err) {
		t.Errorf("expected claude host dir to be absent when disabled, stat err = %v", err)
	}
	sharedHost := filepath.Join(home, ".fleet", "workspaces", "beta", "files")
	if _, err := os.Stat(sharedHost); !os.IsNotExist(err) {
		t.Errorf("expected shared files dir to be absent when no file mounts, stat err = %v", err)
	}
}

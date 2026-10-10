package resolver

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// allMountsSettings enables every mount the resolver knows, plus custom mounts
// in non-canonical spellings, so the Layout tests cover each spec.
func allMountsSettings() fleet.FleetSettings {
	return fleet.FleetSettings{
		ClaudeCodeMount: true,
		CodexMount:      true,
		GhMount:         true,
		AuggieMount:     true,
		CustomMounts:    []string{"/opt/data/", "/var//cache/./shared"},
	}
}

// TestResolveLayoutListsEveryCreatedPath pins the Layout a fully enabled fleet
// gets, in creation order, and checks the host tree matches it entry by entry:
// kind and mode. A backend replaying Layout elsewhere must end up with the same
// tree the resolver made on the host.
func TestResolveLayoutListsEveryCreatedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	resolved, err := Resolve("alpha", allMountsSettings())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	want := []LayoutEntry{
		{Path: ".claude", Dir: true, Mode: 0777},
		{Path: ".codex", Dir: true, Mode: 0777},
		{Path: ".config/gh", Dir: true, Mode: 0777},
		{Path: ".augment", Dir: true, Mode: 0777},
		{Path: ".mnt/opt/data", Dir: true, Mode: 0777},
		{Path: ".mnt/var/cache/shared", Dir: true, Mode: 0777},
		{Path: "files", Dir: true, Mode: 0777},
		{Path: "files/.claude.json", Mode: 0666},
		{Path: "files/claude-settings.local.json", Mode: 0666},
	}
	if !slices.Equal(resolved.Layout, want) {
		t.Fatalf("Layout =\n%+v\nwant\n%+v", resolved.Layout, want)
	}

	root := fleetMountDir("alpha")
	for _, entry := range resolved.Layout {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Errorf("Layout entry %s not on the host: %v", entry.Path, err)
			continue
		}
		if entry.Dir && !info.IsDir() || !entry.Dir && !info.Mode().IsRegular() {
			t.Errorf("%s: host mode %v, want Dir=%v", entry.Path, info.Mode(), entry.Dir)
		}
		if got := info.Mode().Perm(); got != entry.Mode {
			t.Errorf("%s: host perm %v, Layout says %v", entry.Path, got, entry.Mode)
		}
	}
}

// TestResolveMountsAreFleetSharedSubPaths checks the metadata a backend
// without the daemon host's filesystem relies on: every mount is fleet-shared,
// its SubPath is a clean relative path naming a Layout directory, and its
// LocalPath is exactly the fleet's mount root joined with that SubPath.
func TestResolveMountsAreFleetSharedSubPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	resolved, err := Resolve("alpha", allMountsSettings())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolved.Mounts) == 0 {
		t.Fatal("Resolve() returned no mounts")
	}

	layoutDirs := map[string]bool{}
	for _, entry := range resolved.Layout {
		if entry.Dir {
			layoutDirs[entry.Path] = true
		}
	}
	root := fleetMountDir("alpha")
	for _, mount := range resolved.Mounts {
		if mount.Scope != backend.MountScopeFleetShared {
			t.Errorf("%s: Scope = %v, want %v", mount.ContainerPath, mount.Scope, backend.MountScopeFleetShared)
		}
		if mount.ReadOnly {
			t.Errorf("%s: ReadOnly = true, want false", mount.ContainerPath)
		}
		if !layoutDirs[mount.SubPath] {
			t.Errorf("%s: SubPath %q is not a Layout directory", mount.ContainerPath, mount.SubPath)
		}
		if want := filepath.Join(root, filepath.FromSlash(mount.SubPath)); mount.LocalPath != want {
			t.Errorf("%s: LocalPath = %q, want root + SubPath = %q", mount.ContainerPath, mount.LocalPath, want)
		}
	}
}

// TestResolveLayoutPathsStayBelowTheRoot guards the Layout contract replaying
// backends depend on: relative, clean, slash-separated, never "..", whatever
// spelling the custom mounts were saved in.
func TestResolveLayoutPathsStayBelowTheRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	settings := allMountsSettings()
	settings.CustomMounts = append(settings.CustomMounts, " /srv/x ", "/a/./b//c/", "../../escape", "relative")
	resolved, err := Resolve("alpha", settings)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	for _, entry := range resolved.Layout {
		p := entry.Path
		if p == "" || path.IsAbs(p) || path.Clean(p) != p || slices.Contains(strings.Split(p, "/"), "..") {
			t.Errorf("Layout path %q is not a clean relative path below the root", p)
		}
	}
}

func TestResolveLayoutEmptyWhenNoSettingsEnabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	resolved, err := Resolve("alpha", fleet.FleetSettings{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolved.Layout) != 0 {
		t.Fatalf("Layout = %+v, want empty", resolved.Layout)
	}
}

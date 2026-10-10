package resolver

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// symlinkCase plants a symlink at one Layout path and names the settings that
// make Resolve reach it.
type symlinkCase struct {
	name     string
	settings fleet.FleetSettings
	// link is the symlink's path relative to the fleet's mount root.
	link string
	// targetIsFile points the link at a file instead of a folder.
	targetIsFile bool
	// dangling points the link at a path that does not exist.
	dangling bool
	// wantErr is the whole Resolve error, with <root> standing for
	// ~/.fleet/workspaces/<fleet>.
	wantErr string
}

// TestResolveRefusesSymlinkedLayoutPaths is the regression test for the
// resolver following symlinks: a symlink at any Layout path, or at a folder
// above one, fails Resolve with a message naming the path and the fix, and
// the link's target is neither chmodded, created nor written.
func TestResolveRefusesSymlinkedLayoutPaths(t *testing.T) {
	cases := []symlinkCase{
		{
			name:     "custom mount folder",
			settings: fleet.FleetSettings{CustomMounts: []string{"/opt/data"}},
			link:     ".mnt/opt/data",
			wantErr:  "custom mount /opt/data: <root>/.mnt/opt/data is a symlink; pick a host folder for this mount in the fleet options instead",
		},
		{
			name:     "folder above a custom mount",
			settings: fleet.FleetSettings{CustomMounts: []string{"/opt/data"}},
			link:     ".mnt",
			wantErr:  "custom mount /opt/data: <root>/.mnt is a symlink; pick a host folder for this mount in the fleet options instead",
		},
		{
			name:     "dangling custom mount folder",
			settings: fleet.FleetSettings{CustomMounts: []string{"/opt/data"}},
			link:     ".mnt/opt/data",
			dangling: true,
			wantErr:  "custom mount /opt/data: <root>/.mnt/opt/data is a symlink; pick a host folder for this mount in the fleet options instead",
		},
		{
			name:     "managed mount folder",
			settings: fleet.FleetSettings{CodexMount: true},
			link:     ".codex",
			wantErr:  "preparing Codex mount: <root>/.codex is a symlink; replace it with a real folder",
		},
		{
			name:     "folder above a managed mount",
			settings: fleet.FleetSettings{GhMount: true},
			link:     ".config",
			wantErr:  "preparing GitHub CLI mount: <root>/.config is a symlink; replace it with a real folder",
		},
		{
			name:     "shared files folder",
			settings: fleet.FleetSettings{ClaudeCodeMount: true},
			link:     "files",
			wantErr:  "preparing shared files mount: <root>/files is a symlink; replace it with a real folder",
		},
		{
			name:         "shared file",
			settings:     fleet.FleetSettings{ClaudeCodeMount: true},
			link:         "files/.claude.json",
			targetIsFile: true,
			wantErr:      "preparing Claude Code config file: <root>/files/.claude.json is a symlink; replace it with a regular file",
		},
		{
			name:     "dangling shared file",
			settings: fleet.FleetSettings{ClaudeCodeMount: true},
			link:     "files/claude-settings.local.json",
			dangling: true,
			wantErr:  "preparing Claude Code project settings file: <root>/files/claude-settings.local.json is a symlink; replace it with a regular file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := fleetMountDir("delta")

			outside := t.TempDir()
			target := filepath.Join(outside, "target")
			switch {
			case tc.dangling:
			case tc.targetIsFile:
				if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			}
			linkPath := filepath.Join(root, filepath.FromSlash(tc.link))
			if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, linkPath); err != nil {
				t.Fatal(err)
			}

			resolved, err := Resolve("delta", tc.settings)
			if err == nil {
				t.Fatalf("Resolve() = %+v, want a symlink refusal", resolved)
			}
			want := strings.ReplaceAll(tc.wantErr, "<root>", "~/.fleet/workspaces/delta")
			if err.Error() != want {
				t.Errorf("Resolve() error =\n  %s\nwant\n  %s", err, want)
			}
			var symlinkErr *SymlinkError
			if !errors.As(err, &symlinkErr) || symlinkErr.Path != linkPath {
				t.Errorf("Resolve() error = %#v, want a *SymlinkError for %s", err, linkPath)
			}
			if len(resolved.Mounts) != 0 || len(resolved.Symlinks) != 0 {
				t.Errorf("Resolve() returned %+v alongside the error", resolved)
			}

			if info, err := os.Lstat(linkPath); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Errorf("the symlink at %s was not left in place: %v, %v", linkPath, info, err)
			}
			if tc.dangling {
				if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("dangling target %s was created: %v", target, err)
				}
				return
			}
			assertMode(t, target, map[bool]fs.FileMode{true: 0600, false: 0700}[tc.targetIsFile])
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Errorf("Resolve wrote beside the link target: %v, %v", entries, err)
			}
			if tc.targetIsFile {
				if got, _ := os.ReadFile(target); string(got) != "secret" {
					t.Errorf("link target rewritten: %q", got)
				}
			} else if entries, _ := os.ReadDir(target); len(entries) != 0 {
				t.Errorf("Resolve created %v inside the link target", entries)
			}
		})
	}
}

// TestResolveLeavesSymlinksBelowLayoutPaths verifies the refusal stops at
// Layout paths: symlinks users keep inside a mount folder are theirs, so
// Resolve neither refuses nor touches them.
func TestResolveLeavesSymlinksBelowLayoutPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := fleetMountDir("delta")
	settings := fleet.FleetSettings{ClaudeCodeMount: true, CustomMounts: []string{"/opt/data"}}
	if _, err := Resolve("delta", settings); err != nil {
		t.Fatalf("first Resolve() error = %v", err)
	}

	outside := t.TempDir()
	if err := os.Chmod(outside, 0700); err != nil {
		t.Fatal(err)
	}
	links := []string{
		filepath.Join(root, ".claude", "projects"),
		filepath.Join(root, ".mnt", "opt", "data", "big-disk"),
	}
	for _, link := range links {
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Resolve("delta", settings); err != nil {
		t.Fatalf("Resolve() with user symlinks below Layout paths: %v", err)
	}
	for _, link := range links {
		if got, err := os.Readlink(link); err != nil || got != outside {
			t.Errorf("user symlink %s changed: %q, %v", link, got, err)
		}
	}
	assertMode(t, outside, 0700)
}

// TestResolveFollowsSymlinkedFleetRoot pins the boundary from the other side:
// the fleet's mount root and the folders above it belong to the user, so a
// fleet folder moved to another disk behind a symlink keeps provisioning.
func TestResolveFollowsSymlinkedFleetRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(fleetMountDir("delta")), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, fleetMountDir("delta")); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve("delta", fleet.FleetSettings{CodexMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolved.Mounts) != 1 || resolved.Mounts[0].LocalPath != filepath.Join(fleetMountDir("delta"), ".codex") {
		t.Fatalf("Resolve() mounts = %+v", resolved.Mounts)
	}
	assertMode(t, filepath.Join(elsewhere, ".codex"), 0777)
}

// TestResolveSetsLayoutPermissions verifies the permission contract survives
// the switch away from MkdirAll/Chmod: mount folders end up 0777 and shared
// files 0666 whatever the umask, older 0700/0600 entries are upgraded, and the
// folders created above a mount folder keep their umask-applied mode.
func TestResolveSetsLayoutPermissions(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022))
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := fleetMountDir("delta")
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "files", ".claude.json"), []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve("delta", fleet.FleetSettings{
		ClaudeCodeMount: true,
		GhMount:         true,
		CustomMounts:    []string{"/opt/data"},
	}); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	for _, dir := range []string{".claude", ".config/gh", ".mnt/opt/data", "files"} {
		assertMode(t, filepath.Join(root, filepath.FromSlash(dir)), 0777)
	}
	for _, file := range []string{".claude.json", "claude-settings.local.json"} {
		path := filepath.Join(root, "files", file)
		assertMode(t, path, 0666)
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s is not a regular file: %v, %v", path, info, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(root, "files", ".claude.json")); string(got) != `{"a":1}` {
		t.Errorf("existing shared file rewritten: %q", got)
	}
	for _, parent := range []string{".config", ".mnt", ".mnt/opt"} {
		assertMode(t, filepath.Join(root, filepath.FromSlash(parent)), 0755)
	}
}

// TestResolveRefusesNonDirectoryAtLayoutFolder keeps MkdirAll's refusal of a
// file where a mount folder belongs.
func TestResolveRefusesNonDirectoryAtLayoutFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := fleetMountDir("delta")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve("delta", fleet.FleetSettings{CodexMount: true})
	if want := "preparing Codex mount: ~/.fleet/workspaces/delta/.codex is not a directory"; err == nil || err.Error() != want {
		t.Fatalf("Resolve() error = %v, want %q", err, want)
	}
}

// TestResolveConcurrentProvisionsOfOneFleet covers two instances of one fleet
// provisioning at once: both create the same Layout paths, and the loser of
// each mkdir/create race must not fail.
func TestResolveConcurrentProvisionsOfOneFleet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	settings := fleet.FleetSettings{
		ClaudeCodeMount: true,
		CodexMount:      true,
		GhMount:         true,
		AuggieMount:     true,
		CustomMounts:    []string{"/opt/data", "/opt/data/nested", "/var/cache/shared"},
	}

	const workers = 16
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Resolve("delta", settings)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Resolve() #%d error = %v", i, err)
		}
	}
}

// TestDisplayPathAbbreviatesHome checks the "~" rendering used in refusals.
func TestDisplayPathAbbreviatesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]string{
		filepath.Join(home, ".fleet", "x"): filepath.Join("~", ".fleet", "x"),
		home + "-other/.fleet":             home + "-other/.fleet",
		"/srv/fleet/x":                     "/srv/fleet/x",
	}
	for in, want := range cases {
		if got := displayPath(in); got != want {
			t.Errorf("displayPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// assertMode fails the test unless path's permission bits are want.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Errorf("lstat %s: %v", path, err)
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %v, want %v", path, got, want)
	}
}

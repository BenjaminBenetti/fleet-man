//go:build unix

package podshare

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// claudeLayout is the layout a fleet with Claude Code's mounts and one
// custom mount gets (the resolver's, as fleetd hands it over).
func claudeLayout(uid, gid int) Layout {
	return Layout{V: LayoutVersion, UID: uid, GID: gid, Entries: []Entry{
		{Path: ".claude", Dir: true, Mode: 0o777},
		{Path: ".mnt/opt/data", Dir: true, Mode: 0o777},
		{Path: "files", Dir: true, Mode: 0o777},
		{Path: "files/.claude.json", Mode: 0o666, Seed: "{}"},
		{Path: "files/claude-settings.local.json", Mode: 0o666, Seed: "{}"},
		{Path: "files/plain.txt", Mode: 0o666},
	}}
}

func lstat(t *testing.T, p string) fs.FileInfo {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestApplyMakesTheTree(t *testing.T) {
	dir := t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()
	if err := Apply(dir, claudeLayout(uid, gid)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range []struct {
		path string
		dir  bool
		mode fs.FileMode
	}{
		{"roots", true, 0o755},
		{"state", true, 0o700},
		{"lease", false, 0o600},
		{"roots/.claude", true, 0o777},
		{"roots/.mnt", true, 0o755},
		{"roots/.mnt/opt", true, 0o755},
		{"roots/.mnt/opt/data", true, 0o777},
		{"roots/files", true, 0o777},
		{"roots/files/.claude.json", false, 0o666},
	} {
		info := lstat(t, filepath.Join(dir, c.path))
		if info.IsDir() != c.dir || info.Mode().Perm() != c.mode {
			t.Errorf("%s: %v, want dir=%v mode %o", c.path, info.Mode(), c.dir, c.mode)
		}
		if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != uid {
			t.Errorf("%s: owned by %d, want %d", c.path, st.Uid, uid)
		}
	}
	for _, f := range []string{"roots/files/.claude.json", "roots/files/claude-settings.local.json"} {
		if data, _ := os.ReadFile(filepath.Join(dir, f)); string(data) != "{}" {
			t.Errorf("%s holds %q, want the '{}' seed", f, data)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "roots/files/plain.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file without a seed was created (%v): an empty one would read as content", err)
	}
}

func TestApplyKeepsContentAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	l := claudeLayout(-1, -1)
	if err := Apply(dir, l); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "roots/files/.claude.json")
	if err := os.WriteFile(keep, []byte(`{"user":"logged in"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(dir, "roots/.claude/projects/p.jsonl")
	if err := os.MkdirAll(filepath.Dir(inner), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inner, []byte("transcript"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Apply(dir, l); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if data, _ := os.ReadFile(keep); string(data) != `{"user":"logged in"}` {
		t.Errorf("Apply replaced an existing file's content with %q", data)
	}
	if info := lstat(t, keep); info.Mode().Perm() != 0o666 {
		t.Errorf("an existing file kept mode %o, want the layout's 666", info.Mode().Perm())
	}
	if data, _ := os.ReadFile(inner); string(data) != "transcript" {
		t.Errorf("Apply touched content below a layout path: %q", data)
	}
}

// TestApplyReplacesSymlinks: a link at or above a layout path (one another
// pod of the fleet planted) is removed, never followed: the kubelet would
// mount whatever it names.
func TestApplyReplacesSymlinks(t *testing.T) {
	for _, at := range []string{"roots", "roots/.claude", "roots/.mnt", "roots/.mnt/opt/data", "state", "lease", "roots/files/.claude.json"} {
		t.Run(at, func(t *testing.T) {
			dir := t.TempDir()
			if err := Apply(dir, claudeLayout(-1, -1)); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			target := filepath.Join(outside, "victim")
			if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, at)
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if err := Apply(dir, claudeLayout(-1, -1)); err != nil {
				t.Fatalf("Apply over a symlink at %s: %v", at, err)
			}
			if info := lstat(t, link); info.Mode()&fs.ModeSymlink != 0 {
				t.Fatalf("%s is still a symlink", at)
			}
			if data, _ := os.ReadFile(target); string(data) != "outside" {
				t.Fatalf("Apply wrote through the link: %q", data)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 1 {
				t.Fatalf("Apply made %d entries outside the share", len(entries)-1)
			}
		})
	}
}

func TestApplyRefusesAFileWhereAFolderBelongs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "roots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roots/.claude"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Apply(dir, claudeLayout(-1, -1))
	if err == nil || !strings.Contains(err.Error(), "roots/.claude is a file where the fleet's folder belongs") {
		t.Fatalf("Apply = %v, want the folder refused by name", err)
	}
}

func TestLayoutValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Layout
		ok   bool
	}{
		{"claude", claudeLayout(1000, 1000), true},
		{"no owner", claudeLayout(-1, -1), true},
		{"newer", Layout{V: LayoutVersion + 1}, false},
		{"absolute", Layout{Entries: []Entry{{Path: "/etc", Dir: true}}}, false},
		{"dot-dot", Layout{Entries: []Entry{{Path: ".mnt/../../x", Dir: true}}}, false},
		{"set-uid", Layout{Entries: []Entry{{Path: "a", Mode: 0o4755}}}, false},
		{"a seeded folder", Layout{Entries: []Entry{{Path: "a", Dir: true, Seed: "{}"}}}, false},
		{"half an owner", Layout{UID: 1000, GID: -1}, false},
	} {
		if err := tc.l.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestLoadLayout(t *testing.T) {
	p := filepath.Join(t.TempDir(), LayoutFile)
	data, _ := json.Marshal(claudeLayout(1000, 1000))
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLayout(p)
	if err != nil || !reflect.DeepEqual(l, claudeLayout(1000, 1000)) {
		t.Fatalf("LoadLayout = %+v, %v", l, err)
	}
	if err := os.WriteFile(p, []byte(`{"v":1,"entries":[{"path":"../x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLayout(p); err == nil {
		t.Fatal("LoadLayout took a path out of the tree")
	}
}

func TestSubPath(t *testing.T) {
	if got := SubPath(".mnt/opt/data"); got != "roots/.mnt/opt/data" {
		t.Fatalf("SubPath = %q", got)
	}
}

package podseed

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPlanRoot(t *testing.T) {
	tops := map[string]topKind{
		"etc": topDir, "usr": topDir, "var": topDir, "home": topDir, "opt": topDir, "tmp": topDir,
		"proc": topDir, "sys": topMount, "dev": topMount,
		"bin": topOther, "lib": topOther, "app.jar": topOther,
		"fleet-pvc": topMount, "fleet-tools": topMount, "cache": topDir,
	}
	plan := planRoot(tops, SeedSpec{
		Exclude: []string{"cache"},
		MountDirs: []string{
			"/workspaces/repo",      // no /workspaces in the image: root-extra
			"/workspaces/repo/.git", // nested: its parent mount provides it
			"/var/lib/docker",       // inside a copied dir: a mountpoint
			"/opt/fleet", "/opt/fleet/spec", "/fleet-mounts/control",
			"/dev/shm",       // the runtime's
			"/bin/tool",      // /bin is a symlink in the image
			"/fleet-tools/x", // the seed pod's own mount is not the image's
			"/data",
		},
	})
	if want := []string{"etc", "home", "opt", "tmp", "usr", "var"}; !reflect.DeepEqual(plan.dirs, want) {
		t.Errorf("dirs %v, want %v", plan.dirs, want)
	}
	if want := []string{"data", "fleet-mounts", "fleet-tools", "workspaces"}; !reflect.DeepEqual(plan.extra, want) {
		t.Errorf("extra %v, want %v", plan.extra, want)
	}
	if want := map[string]bool{"/var/lib/docker": true, "/opt/fleet": true}; !reflect.DeepEqual(plan.mountpoints, want) {
		t.Errorf("mountpoints %v, want %v", plan.mountpoints, want)
	}
	if len(plan.warnings) != 1 || !strings.Contains(plan.warnings[0], "/bin/tool") {
		t.Errorf("warnings %v", plan.warnings)
	}
}

func TestOutermost(t *testing.T) {
	got := outermost([]string{"/a/b", "/a", "/ab", "/a/b/c", "/x/", "/a"})
	if want := []string{"/a", "/ab", "/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// makeGen lays out a generation on a temp PVC: tree entries under root/ (a
// value starting "->" is a symlink) and the image's top-level symlinks.
func makeGen(t *testing.T, tree map[string]string, topLinks map[string]string) genRoot {
	t.Helper()
	pvc := t.TempDir()
	g := genRoot{pvc: pvc, dirs: map[string]bool{}, extra: map[string]bool{"workspaces": true}, topLinks: topLinks}
	if err := os.MkdirAll(filepath.Join(pvc, DirRootExtra, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, v := range tree {
		g.dirs[strings.SplitN(p, "/", 2)[0]] = true
		full := filepath.Join(pvc, DirRoot, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasPrefix(v, "->"):
			if err := os.Symlink(strings.TrimPrefix(v, "->"), full); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(p, "/"):
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(full, []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

func TestGenRootResolve(t *testing.T) {
	g := makeGen(t, map[string]string{
		"etc/passwd":       "root:x:0:0::/root:/bin/sh\n",
		"etc/alt":          "->/usr/share/alt",
		"usr/share/alt":    "alt",
		"usr/bin/":         "",
		"var/home/vscode/": "",
		"var/escape":       "->../../../../../../../etc",
		"var/loop":         "->loop2",
		"var/loop2":        "->loop",
		"var/rel":          "->home/vscode",
	}, map[string]string{"home": "var/home", "bin": "usr/bin", "abs": "/usr"})
	root := filepath.Join(g.pvc, DirRoot)
	for in, want := range map[string]string{
		"/etc/passwd":          filepath.Join(root, "etc/passwd"),
		"/etc/alt":             filepath.Join(root, "usr/share/alt"),
		"/home/vscode":         filepath.Join(root, "var/home/vscode"),
		"/bin":                 filepath.Join(root, "usr/bin"),
		"/bin/fleet":           filepath.Join(root, "usr/bin/fleet"), // missing last component: where it would go
		"/abs/bin":             filepath.Join(root, "usr/bin"),
		"/var/rel":             filepath.Join(root, "var/home/vscode"),
		"/var/escape/passwd":   filepath.Join(root, "etc/passwd"), // `..` stops at /
		"/usr/../etc/./passwd": filepath.Join(root, "etc/passwd"),
		"/workspaces":          filepath.Join(g.pvc, DirRootExtra, "workspaces"),
	} {
		got, err := g.resolve(in)
		if err != nil || got != want {
			t.Errorf("resolve(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"relative", "/var/loop/x", "/nowhere/x", "/etc/passwd/x", "/var/missing/x", "/"} {
		if got, err := g.resolve(bad); err == nil {
			t.Errorf("resolve(%s) = %q, want an error", bad, got)
		}
	}
	if _, err := g.resolve("/nowhere/x"); !errors.Is(err, errNoRoot) {
		t.Errorf("a path outside the generation: %v", err)
	}
	if _, err := g.resolveDir("/etc/passwd"); err == nil {
		t.Error("resolveDir took a file")
	}
	if dir, err := g.resolveDir("/home/vscode"); err != nil || !strings.HasSuffix(dir, "var/home/vscode") {
		t.Errorf("resolveDir = %q, %v", dir, err)
	}
	if _, err := g.resolve("/var/missing/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing parent: %v", err)
	}
}

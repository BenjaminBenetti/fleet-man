//go:build unix

package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture is a Root with symlinks of every kind planted in it, beside a folder
// outside it that no operation may change. Cleanup fails the test when the
// outside folder, or the folder inside the root that only links point at,
// changed.
//
//	base/
//	  outside/            secret (file), sub/ (folder)
//	  root/               the Root
//	    a/                f (file), b/ (folder), up -> ../../outside
//	    inside/           only links point here
//	    abs  -> base/outside              rel    -> ../outside
//	    in   -> inside                    dang   -> nowhere
//	    file -> base/outside/secret       filein -> a/f
type fixture struct {
	base    string
	root    string
	outside string
	r       *Root
}

// folderLinks are the fixture's symlinks to folders (and the dangling one),
// for paths that go through them.
var folderLinks = []string{"abs", "rel", "in", "dang", "a/up"}

// fileLinks are the fixture's symlinks to files.
var fileLinks = []string{"file", "filein"}

// eachWalk runs fn once with the Linux openat2 fast path (where this system
// has it) and once with the portable element walk.
func eachWalk(t *testing.T, fn func(t *testing.T, portable bool)) {
	t.Run("fast", func(t *testing.T) {
		if !FastPath() {
			t.Skip("no openat2 fast path on this system")
		}
		fn(t, false)
	})
	t.Run("portable", func(t *testing.T) { fn(t, true) })
}

// openTest opens dir as a Root using the chosen walk, closed at cleanup.
func openTest(t *testing.T, dir string, opts Options, portable bool) *Root {
	t.Helper()
	r, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	r.portable = portable
	t.Cleanup(func() { r.Close() })
	return r
}

func newFixture(t *testing.T, portable bool) *fixture {
	t.Helper()
	base := t.TempDir()
	fx := &fixture{base: base, root: filepath.Join(base, "root"), outside: filepath.Join(base, "outside")}
	must(t, os.MkdirAll(filepath.Join(fx.outside, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(fx.outside, "secret"), []byte("secret"), 0o600))
	must(t, os.MkdirAll(filepath.Join(fx.root, "a", "b"), 0o755))
	must(t, os.Mkdir(filepath.Join(fx.root, "inside"), 0o755))
	must(t, os.WriteFile(filepath.Join(fx.root, "a", "f"), []byte("f"), 0o644))
	for link, target := range map[string]string{
		"abs":    fx.outside,
		"rel":    "../outside",
		"in":     "inside",
		"dang":   "nowhere",
		"file":   filepath.Join(fx.outside, "secret"),
		"filein": "a/f",
		"a/up":   "../../outside",
	} {
		must(t, os.Symlink(target, filepath.Join(fx.root, filepath.FromSlash(link))))
	}
	outsideBefore := mustSnapshot(t, fx.outside)
	insideBefore := mustSnapshot(t, filepath.Join(fx.root, "inside"))
	t.Cleanup(func() {
		if after := mustSnapshot(t, fx.outside); after != outsideBefore {
			t.Errorf("the folder outside the root changed:\n%s\nwas\n%s", after, outsideBefore)
		}
		if after := mustSnapshot(t, filepath.Join(fx.root, "inside")); after != insideBefore {
			t.Errorf("an operation went through a link into inside/:\n%s\nwas\n%s", after, insideBefore)
		}
	})
	fx.r = openTest(t, fx.root, Options{}, portable)
	return fx
}

// path returns the host path of the fixture root's name.
func (fx *fixture) path(name string) string {
	return filepath.Join(fx.root, filepath.FromSlash(name))
}

func mustSnapshot(t *testing.T, dir string) string {
	t.Helper()
	s, err := snapshot(dir)
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

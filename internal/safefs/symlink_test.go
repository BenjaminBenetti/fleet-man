//go:build unix

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// pathOp is one Root operation on a path.
type pathOp struct {
	name string
	run  func(r *Root, p string) error
}

// throughOps are operations on a path below a folder element, each of which
// must refuse a symlink at that element.
var throughOps = []pathOp{
	{"Lstat", func(r *Root, p string) error { _, err := r.Lstat(p + "/secret"); return err }},
	{"Open", func(r *Root, p string) error { return closeFile(r.Open(p + "/secret")) }},
	{"OpenFile create", func(r *Root, p string) error {
		return closeFile(r.OpenFile(p+"/new", os.O_WRONLY|os.O_CREATE, 0o644))
	}},
	{"OpenFile create excl", func(r *Root, p string) error {
		return closeFile(r.OpenFile(p+"/new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644))
	}},
	{"OpenFile truncate", func(r *Root, p string) error {
		return closeFile(r.OpenFile(p+"/secret", os.O_WRONLY|os.O_TRUNC, 0))
	}},
	{"ReadFile", func(r *Root, p string) error { _, err := r.ReadFile(p + "/secret"); return err }},
	{"WriteFile", func(r *Root, p string) error {
		_, err := r.WriteFile(p+"/secret", strings.NewReader("pwned"), 0o666)
		return err
	}},
	{"Mkdir", func(r *Root, p string) error { return r.Mkdir(p+"/new", 0o777) }},
	{"MkdirAll", func(r *Root, p string) error { return r.MkdirAll(p+"/new/deeper", 0o777) }},
	{"Remove", func(r *Root, p string) error { return r.Remove(p + "/secret") }},
	{"RemoveAll file", func(r *Root, p string) error { return r.RemoveAll(p + "/secret") }},
	{"RemoveAll folder", func(r *Root, p string) error { return r.RemoveAll(p + "/sub") }},
	{"Rename from", func(r *Root, p string) error { return r.Rename(p+"/secret", "moved") }},
	{"Rename to", func(r *Root, p string) error { return r.Rename("a/f", p+"/new") }},
	{"Rename within", func(r *Root, p string) error { return r.Rename(p+"/secret", p+"/moved") }},
	{"Symlink", func(r *Root, p string) error { return r.Symlink("x", p+"/new") }},
	{"Readlink", func(r *Root, p string) error { _, err := r.Readlink(p + "/secret"); return err }},
	{"Chmod", func(r *Root, p string) error { return r.Chmod(p+"/secret", 0o777) }},
	{"Chtimes", func(r *Root, p string) error { return r.Chtimes(p+"/secret", time.Unix(1, 0), time.Unix(1, 0)) }},
	{"Lchown", func(r *Root, p string) error { return r.Lchown(p+"/secret", os.Getuid(), os.Getgid()) }},
	{"ReadDir", func(r *Root, p string) error { _, err := r.ReadDir(p); return err }},
	{"ReadDir below", func(r *Root, p string) error { _, err := r.ReadDir(p + "/sub"); return err }},
	{"OpenRoot", func(r *Root, p string) error { return closeRoot(r.OpenRoot(p)) }},
	{"WalkDir below", func(r *Root, p string) error {
		return r.WalkDir(p+"/sub", func(_ string, _ fs.DirEntry, err error) error { return err })
	}},
}

// TestOperationsRefuseSymlinkedFolders is the core promise: whatever kind of
// symlink sits at a folder element (absolute or relative to outside, to a
// folder inside the root, dangling, deeper in the tree), every operation
// through it fails with ErrSymlink naming the link, and nothing outside the
// root or behind the in-root link changes (the fixture checks).
func TestOperationsRefuseSymlinkedFolders(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		for _, link := range folderLinks {
			for _, op := range throughOps {
				err := op.run(fx.r, link)
				if !errors.Is(err, ErrSymlink) {
					t.Errorf("%s through %s: error %v, want ErrSymlink", op.name, link, err)
					continue
				}
				var pathErr *fs.PathError
				if !errors.As(err, &pathErr) || pathErr.Path != fx.path(link) {
					t.Errorf("%s through %s: error %v names the wrong path, want %s", op.name, link, err, fx.path(link))
				}
			}
		}
	})
}

// TestOperationsRefuseSymlinkedFiles covers a symlink as the last element of
// an operation that would act on what it points at.
func TestOperationsRefuseSymlinkedFiles(t *testing.T) {
	ops := []pathOp{
		{"Open", func(r *Root, p string) error { return closeFile(r.Open(p)) }},
		{"OpenFile write", func(r *Root, p string) error { return closeFile(r.OpenFile(p, os.O_WRONLY, 0)) }},
		{"OpenFile truncate", func(r *Root, p string) error { return closeFile(r.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)) }},
		{"OpenFile append", func(r *Root, p string) error {
			return closeFile(r.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644))
		}},
		{"ReadFile", func(r *Root, p string) error { _, err := r.ReadFile(p); return err }},
		{"Chmod", func(r *Root, p string) error { return r.Chmod(p, 0o777) }},
		{"Chtimes", func(r *Root, p string) error { return r.Chtimes(p, time.Unix(1, 0), time.Unix(1, 0)) }},
		{"ReadDir", func(r *Root, p string) error { _, err := r.ReadDir(p); return err }},
		{"MkdirAll", func(r *Root, p string) error { return r.MkdirAll(p, 0o777) }},
		{"OpenRoot", func(r *Root, p string) error { return closeRoot(r.OpenRoot(p)) }},
	}
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		for _, link := range append(append([]string{}, fileLinks...), folderLinks...) {
			for _, op := range ops {
				if err := op.run(fx.r, link); !errors.Is(err, ErrSymlink) {
					t.Errorf("%s %s: error %v, want ErrSymlink", op.name, link, err)
				}
			}
		}
		// Creating over any entry, a link included, is an existing entry.
		for _, link := range fileLinks {
			err := closeFile(fx.r.OpenFile(link, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644))
			if !errors.Is(err, fs.ErrExist) {
				t.Errorf("exclusive create over %s: error %v, want ErrExist", link, err)
			}
			if err := fx.r.Mkdir(link, 0o777); !errors.Is(err, fs.ErrExist) {
				t.Errorf("Mkdir over %s: error %v, want ErrExist", link, err)
			}
		}
	})
}

// TestOperationsOnLinksThemselves covers the operations that act on a link
// and must never touch its target: the fixture checks the targets.
func TestOperationsOnLinksThemselves(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		info, err := fx.r.Lstat("abs")
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("Lstat(abs) = %v, %v; want the link", info, err)
		}
		if target, err := fx.r.Readlink("a/up"); err != nil || target != "../../outside" {
			t.Errorf("Readlink(a/up) = %q, %v", target, err)
		}
		if err := fx.r.Lchown("abs", os.Getuid(), os.Getgid()); err != nil {
			t.Errorf("Lchown(abs): %v", err)
		}
		if err := fx.r.Rename("rel", "a/b/rel"); err != nil {
			t.Errorf("Rename(rel): %v", err)
		}
		// Moved one folder down, ../outside now dangles: the link moved, not its target.
		if target, err := os.Readlink(fx.path("a/b/rel")); err != nil || target != "../outside" {
			t.Errorf("moved link = %q, %v", target, err)
		}
		if err := fx.r.Remove("abs"); err != nil {
			t.Errorf("Remove(abs): %v", err)
		}
		if err := fx.r.RemoveAll("in"); err != nil {
			t.Errorf("RemoveAll(in): %v", err)
		}
		if err := fx.r.RemoveAll("a"); err != nil {
			t.Errorf("RemoveAll(a), which holds a link to outside: %v", err)
		}
		if _, err := fx.r.WriteFile("file", strings.NewReader("mine"), 0o644); err != nil {
			t.Errorf("WriteFile over the link file: %v", err)
		}
		if info, err := os.Lstat(fx.path("file")); err != nil || !info.Mode().IsRegular() {
			t.Errorf("file after WriteFile = %v, %v; want a regular file", info, err)
		}
		for _, gone := range []string{"abs", "in", "a"} {
			if _, err := os.Lstat(fx.path(gone)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s still there: %v", gone, err)
			}
		}
	})
}

// TestErrorsNameTheElementAtFault checks the shape callers turn into their own
// messages: an *fs.PathError for the link's host path, wrapping ErrSymlink.
func TestErrorsNameTheElementAtFault(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		err := fx.r.MkdirAll("a/up/x/y", 0o777)
		if want := "open " + fx.path("a/up") + ": is a symlink"; err == nil || err.Error() != want {
			t.Errorf("MkdirAll error = %v, want %q", err, want)
		}
		must(t, os.WriteFile(fx.path("a/plain"), nil, 0o644))
		err = fx.r.MkdirAll("a/plain/x", 0o777)
		if want := "open " + fx.path("a/plain") + ": is not a directory"; err == nil || err.Error() != want {
			t.Errorf("MkdirAll error = %v, want %q", err, want)
		}
		if !errors.Is(err, ErrNotDir) {
			t.Errorf("MkdirAll error = %v, want ErrNotDir", err)
		}
	})
}

func closeFile(f *os.File, err error) error {
	if err == nil {
		f.Close()
	}
	return err
}

func closeRoot(r *Root, err error) error {
	if err == nil {
		r.Close()
	}
	return err
}

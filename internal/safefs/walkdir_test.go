//go:build unix

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestWalkDir checks WalkDir keeps fs.WalkDir's order and skip semantics and
// reports symlinks as entries without descending into them.
func TestWalkDir(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		must(t, os.WriteFile(fx.path("a/b/g"), nil, 0o644))
		must(t, os.MkdirAll(fx.path("a/c/d"), 0o755))
		must(t, os.WriteFile(fx.path("a/c/h"), nil, 0o644))

		walk := func(start string, fn func(name string, d fs.DirEntry) error) (string, error) {
			var seen []string
			err := fx.r.WalkDir(start, func(name string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				mark := ""
				if d.Type()&fs.ModeSymlink != 0 {
					mark = "@"
				} else if d.IsDir() {
					mark = "/"
				}
				seen = append(seen, name+mark)
				if fn != nil {
					return fn(name, d)
				}
				return nil
			})
			return strings.Join(seen, " "), err
		}

		cases := []struct {
			name, start, want string
			fn                func(string, fs.DirEntry) error
		}{
			{"whole tree", ".", "./ a/ a/b/ a/b/g a/c/ a/c/d/ a/c/h a/f a/up@ abs@ dang@ file@ filein@ in@ inside/ rel@", nil},
			{"subfolder", "a/c", "a/c/ a/c/d/ a/c/h", nil},
			{"file", "a/f", "a/f", nil},
			{"link", "abs", "abs@", nil},
			{"SkipDir on a folder", "a", "a/ a/b/ a/c/ a/c/d/ a/c/h a/f a/up@", func(name string, _ fs.DirEntry) error {
				if name == "a/b" {
					return fs.SkipDir
				}
				return nil
			}},
			{"SkipDir on a file", "a", "a/ a/b/ a/b/g a/c/ a/c/d/ a/c/h a/f", func(name string, _ fs.DirEntry) error {
				if name == "a/f" {
					return fs.SkipDir
				}
				return nil
			}},
			{"SkipAll", ".", "./ a/ a/b/", func(name string, _ fs.DirEntry) error {
				if name == "a/b" {
					return fs.SkipAll
				}
				return nil
			}},
		}
		for _, tc := range cases {
			got, err := walk(tc.start, tc.fn)
			if err != nil || got != tc.want {
				t.Errorf("%s: walked %q, %v\nwant %q", tc.name, got, err, tc.want)
			}
		}

		if _, err := walk("missing", nil); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("walk of a missing folder: %v, want ErrNotExist", err)
		}
		if _, err := walk("abs/sub", nil); !errors.Is(err, ErrSymlink) {
			t.Errorf("walk below a link: %v, want ErrSymlink", err)
		}
		stop := errors.New("stop")
		if _, err := walk(".", func(name string, _ fs.DirEntry) error {
			if name == "a/c/h" {
				return stop
			}
			return nil
		}); err != stop {
			t.Errorf("walk returning an error: %v, want it back", err)
		}
	})
}

// TestWalkDirReportsAFolderThatChanged checks a folder whose entry no longer
// matches what opens is reported to fn, not walked.
func TestWalkDirReportsAFolderThatChanged(t *testing.T) {
	fx := newFixture(t, true)
	d := fx.r.top()
	other, err := fx.r.Lstat("inside")
	must(t, err)
	var reported error
	err = walkChild(d, "a", fs.FileInfoToDirEntry(renamed{other, "a"}), func(name string, _ fs.DirEntry, err error) error {
		reported = err
		return nil
	})
	if err != nil || !errors.Is(reported, ErrChanged) {
		t.Errorf("walkChild of a swapped folder: %v, reported %v; want ErrChanged reported", err, reported)
	}
}

// renamed is a FileInfo under another name.
type renamed struct {
	fs.FileInfo
	name string
}

func (r renamed) Name() string { return r.name }

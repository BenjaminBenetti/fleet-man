package safefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SelfTest checks, in a scratch folder it makes under dir and removes again,
// that this system gives safefs what it relies on: each kind of symlink
// planted in a tree (to a folder outside, relative and absolute; to a file
// outside; to a folder inside; dangling) is refused by every operation that
// would go through it, nothing outside the tree changes, and plain writes,
// renames and walks work. sharesync runs it on the folder it is about to
// sync and refuses to start when it fails; the error names the first check
// that did not hold.
func SelfTest(dir string) error {
	return selfTest(dir, Options{}, false)
}

func selfTest(dir string, opts Options, portable bool) error {
	// Named like a temp file, so a scanner of dir skips it.
	scratch, err := os.MkdirTemp(dir, TempPrefix+"selftest-*"+tempSuffix)
	if err != nil {
		return fmt.Errorf("safefs self-test: %w", err)
	}
	defer os.RemoveAll(scratch)
	outside := filepath.Join(scratch, "outside")
	tree := filepath.Join(scratch, "tree")
	for _, step := range []func() error{
		func() error { return os.Mkdir(outside, 0o700) },
		func() error { return os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600) },
		func() error { return os.MkdirAll(filepath.Join(tree, "inside"), 0o700) },
		func() error { return os.Symlink(outside, filepath.Join(tree, "abs")) },
		func() error { return os.Symlink("../outside", filepath.Join(tree, "rel")) },
		func() error { return os.Symlink(filepath.Join(outside, "secret"), filepath.Join(tree, "file")) },
		func() error { return os.Symlink("inside", filepath.Join(tree, "in")) },
		func() error { return os.Symlink("nowhere", filepath.Join(tree, "dangling")) },
	} {
		if err := step(); err != nil {
			return fmt.Errorf("safefs self-test: preparing: %w", err)
		}
	}
	before, err := snapshot(outside)
	if err != nil {
		return fmt.Errorf("safefs self-test: %w", err)
	}

	r, err := Open(tree, opts)
	if err != nil {
		return fmt.Errorf("safefs self-test: %w", err)
	}
	r.portable = portable
	defer r.Close()
	for _, check := range selfTestChecks(r) {
		if err := check.run(); !errors.Is(err, check.want) {
			return fmt.Errorf("safefs self-test: %s: got %v, want %v", check.name, err, errOrNil(check.want))
		}
	}
	after, err := snapshot(outside)
	if err != nil {
		return fmt.Errorf("safefs self-test: %w", err)
	}
	if before != after {
		return fmt.Errorf("safefs self-test: the folder outside the tree changed:\n%s\nwant\n%s", after, before)
	}
	if entries, err := os.ReadDir(filepath.Join(tree, "inside")); err != nil || len(entries) != 0 {
		return fmt.Errorf("safefs self-test: an operation went through the link to a folder inside the tree: %v, %v", entries, err)
	}
	return nil
}

// selfTestCheck is one operation SelfTest runs and the error it must give.
type selfTestCheck struct {
	name string
	run  func() error
	want error
}

func selfTestChecks(r *Root) []selfTestCheck {
	var checks []selfTestCheck
	for _, link := range []string{"abs", "rel", "in", "dangling"} {
		checks = append(checks,
			selfTestCheck{"write through the folder link " + link, func() error {
				_, err := r.WriteFile(link+"/x", strings.NewReader("x"), 0o666)
				return err
			}, ErrSymlink},
			selfTestCheck{"create through the folder link " + link, func() error {
				f, err := r.OpenFile(link+"/x", os.O_WRONLY|os.O_CREATE, 0o666)
				if err == nil {
					f.Close()
				}
				return err
			}, ErrSymlink},
			selfTestCheck{"mkdir through the folder link " + link, func() error { return r.MkdirAll(link+"/a/b", 0o777) }, ErrSymlink},
			selfTestCheck{"chmod the link " + link, func() error { return r.Chmod(link, 0o777) }, ErrSymlink},
			selfTestCheck{"chtimes the link " + link, func() error { return r.Chtimes(link, time.Unix(1, 0), time.Unix(1, 0)) }, ErrSymlink},
			selfTestCheck{"read through the folder link " + link, func() error { _, err := r.ReadDir(link); return err }, ErrSymlink},
			selfTestCheck{"remove below the folder link " + link, func() error { return r.RemoveAll(link + "/secret") }, ErrSymlink},
		)
	}
	checks = append(checks,
		selfTestCheck{"open the file link", func() error { _, err := r.ReadFile("file"); return err }, ErrSymlink},
		selfTestCheck{"truncate through the file link", func() error {
			f, err := r.OpenFile("file", os.O_WRONLY|os.O_TRUNC, 0)
			if err == nil {
				f.Close()
			}
			return err
		}, ErrSymlink},
		selfTestCheck{"refuse a path with ..", func() error {
			_, err := r.WriteFile("inside/../plain", strings.NewReader("x"), 0o644)
			return err
		}, fs.ErrInvalid},
		selfTestCheck{"write a file", func() error {
			_, err := r.WriteFile("plain", strings.NewReader("plain"), 0o644)
			return err
		}, nil},
		selfTestCheck{"read it back", func() error {
			data, err := r.ReadFile("plain")
			if err == nil && string(data) != "plain" {
				err = fmt.Errorf("read %q", data)
			}
			return err
		}, nil},
		selfTestCheck{"replace the file link without following it", func() error {
			_, err := r.WriteFile("file", strings.NewReader("mine"), 0o644)
			return err
		}, nil},
		selfTestCheck{"rename across folders", func() error {
			if err := r.MkdirAll("a/b", 0o777); err != nil {
				return err
			}
			return r.Rename("plain", "a/b/plain")
		}, nil},
		selfTestCheck{"walk without descending into links", func() error {
			var seen []string
			err := r.WalkDir(".", func(name string, _ fs.DirEntry, err error) error {
				seen = append(seen, name)
				return err
			})
			want := ". a a/b a/b/plain abs dangling file in inside rel"
			if got := strings.Join(seen, " "); err == nil && got != want {
				err = fmt.Errorf("walked %q, want %q", got, want)
			}
			return err
		}, nil},
		selfTestCheck{"remove a link, not its target", func() error { return r.RemoveAll("abs") }, nil},
	)
	return checks
}

// snapshot describes the tree at dir (names, kinds, modes, sizes, link
// targets, modification times and file content) for comparison.
func snapshot(dir string) (string, error) {
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %v %d %d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " -> %s", target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %q", data)
		}
		b.WriteByte('\n')
		return nil
	})
	return b.String(), err
}

// errOrNil renders an expected error for a message.
func errOrNil(err error) any {
	if err == nil {
		return "success"
	}
	return err
}

//go:build unix

package safefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestWriteFileReplacesAtomically covers the hub's write: content and mode
// land, the result is described, no temp file is left, set-ID bits are
// dropped, and a folder at the name is refused.
func TestWriteFileReplacesAtomically(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		info, err := fx.r.WriteFile("a/b/new", strings.NewReader("one"), 0o640)
		if err != nil {
			t.Fatalf("WriteFile(new): %v", err)
		}
		host, err := os.Lstat(fx.path("a/b/new"))
		if err != nil || !os.SameFile(info, host) || info.Name() != "new" {
			t.Errorf("WriteFile info = %v, host %v, %v", info, host, err)
		}
		assertFile(t, fx.path("a/b/new"), "one", 0o640)

		if _, err := fx.r.WriteFile("a/f", strings.NewReader("two"), 0o4755); err != nil {
			t.Fatalf("WriteFile(a/f): %v", err)
		}
		assertFile(t, fx.path("a/f"), "two", 0o755)
		if _, err := fx.r.WriteFile("a/sticky", strings.NewReader("three"), fs.ModeSticky|0o644); err != nil {
			t.Fatalf("WriteFile(a/sticky): %v", err)
		}
		assertFile(t, fx.path("a/sticky"), "three", 0o644)

		if _, err := fx.r.WriteFile("a/b", strings.NewReader("x"), 0o644); !errors.Is(err, syscall.EISDIR) {
			t.Errorf("WriteFile over a folder: %v, want EISDIR", err)
		}
		if _, err := fx.r.WriteFile("missing/x", strings.NewReader("x"), 0o644); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("WriteFile into a missing folder: %v, want ErrNotExist", err)
		}
		if _, err := fx.r.WriteFile("a/c", failingReader{}, 0o644); err == nil {
			t.Error("WriteFile with a failing reader succeeded")
		}
		if _, err := os.Lstat(fx.path("a/c")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a failed WriteFile left a/c: %v", err)
		}
		for _, folder := range []string{"a", "a/b"} {
			entries, _ := os.ReadDir(fx.path(folder))
			for _, e := range entries {
				if IsTemp(e.Name()) {
					t.Errorf("temp file %s left in %s", e.Name(), folder)
				}
			}
		}
	})
}

// TestReplaceFileChecksJustBeforeTheRename covers the precondition: expect
// sees what the name holds after the content is written (nil for nothing),
// its error leaves the name alone with no temp file, and its nil replaces.
func TestReplaceFileChecksJustBeforeTheRename(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		var seen fs.FileInfo
		_, err := fx.r.ReplaceFile("a/f", strings.NewReader("new"), 0o644, time.Time{}, func(current fs.FileInfo) error {
			seen = current
			// A local writer gets in between the content and the rename.
			if err := os.WriteFile(fx.path("a/f"), []byte("theirs"), 0o644); err != nil {
				t.Fatal(err)
			}
			return errMine
		})
		if !errors.Is(err, errMine) {
			t.Fatalf("ReplaceFile with a refusing expect: %v, want its error", err)
		}
		if seen == nil || seen.Name() != "f" {
			t.Errorf("expect saw %v, want a/f", seen)
		}
		assertFile(t, fx.path("a/f"), "theirs", 0o644)
		entries, _ := os.ReadDir(fx.path("a"))
		for _, e := range entries {
			if IsTemp(e.Name()) {
				t.Errorf("temp file %s left", e.Name())
			}
		}

		seen = nil
		called := false
		mtime := time.Date(2020, 1, 2, 3, 4, 5, 6789, time.UTC)
		info, err := fx.r.ReplaceFile("a/fresh", strings.NewReader("one"), 0o600, mtime, func(current fs.FileInfo) error {
			called, seen = true, current
			return nil
		})
		if err != nil {
			t.Fatalf("ReplaceFile(fresh): %v", err)
		}
		if !called || seen != nil {
			t.Errorf("expect for a missing name: called %v with %v, want nil", called, seen)
		}
		assertFile(t, fx.path("a/fresh"), "one", 0o600)
		if !info.ModTime().Equal(mtime) {
			t.Errorf("ReplaceFile mtime = %v, want %v", info.ModTime(), mtime)
		}
	})
}

var errMine = errors.New("not the file I saw")

// TestIsTemp pins the temp-name contract scanners rely on.
func TestIsTemp(t *testing.T) {
	if name := tempName(); !IsTemp(name) || strings.Contains(name, "/") {
		t.Errorf("tempName() = %q", name)
	}
	for _, name := range []string{"x.tmp", ".fleet-safefs-", "settings.json", ".fleet-safefs-1234"} {
		if IsTemp(name) {
			t.Errorf("IsTemp(%q) = true", name)
		}
	}
}

// TestOpenFileFlags checks OpenFile keeps os.OpenFile's flag meanings while
// applying O_TRUNC only after the file is checked.
func TestOpenFileFlags(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)

		f, err := fx.r.OpenFile("a/f", os.O_WRONLY|os.O_APPEND, 0)
		must(t, err)
		_, err = f.WriteString("+")
		must(t, err)
		f.Close()
		assertFile(t, fx.path("a/f"), "f+", 0o644)

		f, err = fx.r.OpenFile("a/f", os.O_WRONLY|os.O_TRUNC, 0)
		must(t, err)
		f.Close()
		assertFile(t, fx.path("a/f"), "", 0o644)

		f, err = fx.r.OpenFile("a/new", os.O_RDWR|os.O_CREATE, 0o600)
		must(t, err)
		f.WriteString("new")
		f.Close()
		assertFile(t, fx.path("a/new"), "new", 0o600)

		f, err = fx.r.OpenFile("a/sticky", os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.ModeSticky|fs.ModeSetgid|0o600)
		must(t, err)
		f.Close()
		assertFile(t, fx.path("a/sticky"), "", 0o600)

		f, err = fx.r.OpenFile("a/new", os.O_RDONLY|os.O_CREATE, 0o644)
		must(t, err)
		data, _ := io.ReadAll(f)
		f.Close()
		if string(data) != "new" {
			t.Errorf("O_CREATE over an existing file read %q", data)
		}
		if err := closeFile(fx.r.OpenFile("a/new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)); !errors.Is(err, fs.ErrExist) {
			t.Errorf("O_EXCL over an existing file: %v", err)
		}
		if err := closeFile(fx.r.OpenFile("a/f", os.O_RDONLY|os.O_SYNC, 0)); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("unsupported flag: %v, want ErrInvalid", err)
		}
		if err := closeFile(fx.r.Open("a/b")); !errors.Is(err, ErrNotRegular) {
			t.Errorf("Open(folder): %v, want ErrNotRegular", err)
		}
		if err := closeFile(fx.r.Open("a/missing")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(missing): %v, want ErrNotExist", err)
		}
	})
}

// TestOpenRefusesSpecialFiles checks a FIFO is refused, not opened (which
// would block until a writer came).
func TestOpenRefusesSpecialFiles(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		must(t, syscall.Mkfifo(fx.path("a/fifo"), 0o644))
		done := make(chan error, 1)
		go func() { done <- closeFile(fx.r.OpenFile("a/fifo", os.O_RDONLY, 0)) }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotRegular) {
				t.Errorf("Open(fifo): %v, want ErrNotRegular", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Open(fifo) blocked")
		}
		if _, err := fx.r.ReadFile("a/fifo"); !errors.Is(err, ErrNotRegular) {
			t.Errorf("ReadFile(fifo): %v", err)
		}
	})
}

// TestMkdirAll creates missing folders with the requested mode before the
// umask, leaves existing ones alone, and tolerates concurrent creators.
func TestMkdirAll(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022))
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		must(t, os.Chmod(fx.path("a"), 0o700))
		must(t, fx.r.MkdirAll("a/x/y/z", 0o777))
		for _, folder := range []string{"a/x", "a/x/y", "a/x/y/z"} {
			assertMode(t, fx.path(folder), fs.ModeDir|0o755)
		}
		assertMode(t, fx.path("a"), fs.ModeDir|0o700)
		must(t, fx.r.MkdirAll(".", 0o777))
		if err := fx.r.MkdirAll("a/f/x", 0o777); !errors.Is(err, ErrNotDir) {
			t.Errorf("MkdirAll through a file: %v, want ErrNotDir", err)
		}
		must(t, fx.r.MkdirAll("a/setgid", 0o777|fs.ModeSetgid))
		assertMode(t, fx.path("a/setgid"), fs.ModeDir|0o755)
		// os.Root refuses a sticky bit at creation; it is dropped (Chmod sets it).
		must(t, fx.r.MkdirAll("a/sticky/x", 0o777|fs.ModeSticky))
		must(t, fx.r.Mkdir("a/sticky/y", 0o777|fs.ModeSticky|fs.ModeSetuid))
		for _, folder := range []string{"a/sticky", "a/sticky/x", "a/sticky/y"} {
			assertMode(t, fx.path(folder), fs.ModeDir|0o755)
		}

		var wg sync.WaitGroup
		errs := make([]error, 16)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = fx.r.MkdirAll("c/d/e/f", 0o755)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("concurrent MkdirAll #%d: %v", i, err)
			}
		}
	})
}

// TestRemoveAndRename covers removes and renames within a folder and across
// folders.
func TestRemoveAndRename(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		must(t, fx.r.MkdirAll("x/y", 0o755))
		must(t, fx.r.Rename("a/f", "x/y/g"))
		assertFile(t, fx.path("x/y/g"), "f", 0o644)
		must(t, fx.r.Rename("x/y/g", "x/y/h"))
		assertFile(t, fx.path("x/y/h"), "f", 0o644)
		must(t, fx.r.Rename("x/y", "a/y"))
		assertFile(t, fx.path("a/y/h"), "f", 0o644)
		if err := fx.r.Remove("a/y"); err == nil {
			t.Error("Remove of a non-empty folder succeeded")
		}
		must(t, fx.r.Remove("a/y/h"))
		must(t, fx.r.Remove("a/y"))
		must(t, fx.r.RemoveAll("a/missing"))
		must(t, fx.r.RemoveAll("missing/deeper"))
		if err := fx.r.Remove("a/missing"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Remove(missing): %v", err)
		}
	})
}

// TestChmodChtimesLchown covers the metadata operations, the root included,
// and a file fleet has no access to.
func TestChmodChtimesLchown(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		must(t, fx.r.Chmod("a/f", 0o600|fs.ModeSetuid))
		assertMode(t, fx.path("a/f"), 0o600)
		must(t, fx.r.Chmod("a/b", fs.ModeSticky|0o700))
		assertMode(t, fx.path("a/b"), fs.ModeDir|fs.ModeSticky|0o700)
		// A sticky bit means nothing on a file (macOS and the BSDs refuse it).
		must(t, fx.r.Chmod("a/f", fs.ModeSticky|0o640))
		assertMode(t, fx.path("a/f"), 0o640)
		must(t, fx.r.Chmod(".", 0o750))
		assertMode(t, fx.root, fs.ModeDir|0o750)

		must(t, os.Chmod(fx.path("a/f"), 0))
		must(t, fx.r.Chmod("a/f", 0o644))
		assertMode(t, fx.path("a/f"), 0o644)

		when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		must(t, fx.r.Chtimes("a/f", when, when))
		must(t, fx.r.Chtimes(".", when, when))
		for _, p := range []string{fx.path("a/f"), fx.root} {
			if info, err := os.Lstat(p); err != nil || !info.ModTime().Equal(when) {
				t.Errorf("%s mtime = %v, %v; want %v", p, info.ModTime(), err, when)
			}
		}
		must(t, fx.r.Lchown("a/f", os.Getuid(), os.Getgid()))
		must(t, fx.r.Lchown(".", os.Getuid(), os.Getgid()))
	})
}

// TestReadDirAndLstat covers listing: sorted, Lstat info, links as links.
func TestReadDirAndLstat(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		fx := newFixture(t, portable)
		entries, err := fx.r.ReadDir("a")
		must(t, err)
		var got []string
		for _, e := range entries {
			info, err := e.Info()
			must(t, err)
			got = append(got, e.Name()+":"+info.Mode().Type().String())
		}
		if want := "b:d--------- f:---------- up:L---------"; strings.Join(got, " ") != want {
			t.Errorf("ReadDir(a) = %q, want %q", strings.Join(got, " "), want)
		}
		if info, err := fx.r.Lstat("."); err != nil || !info.IsDir() {
			t.Errorf("Lstat(.) = %v, %v", info, err)
		}
		if info, err := fx.r.Lstat("a/f"); err != nil || info.Size() != 1 || info.Name() != "f" {
			t.Errorf("Lstat(a/f) = %v, %v", info, err)
		}
	})
}

// TestInvalidNames refuses names outside the io/fs form, and "." for the
// operations that would replace or remove the root.
func TestInvalidNames(t *testing.T) {
	fx := newFixture(t, true)
	for _, name := range []string{"", "/a", "a/", "./a", "a/../b", "..", "a//f"} {
		if _, err := fx.r.Lstat(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Lstat(%q): %v, want ErrInvalid", name, err)
		}
		if _, err := fx.r.WriteFile(name, strings.NewReader("x"), 0o644); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("WriteFile(%q): %v, want ErrInvalid", name, err)
		}
		if err := fx.r.MkdirAll(name, 0o755); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("MkdirAll(%q): %v, want ErrInvalid", name, err)
		}
	}
	for name, op := range map[string]func() error{
		"Remove":    func() error { return fx.r.Remove(".") },
		"RemoveAll": func() error { return fx.r.RemoveAll(".") },
		"Rename":    func() error { return fx.r.Rename(".", "x") },
		"Rename to": func() error { return fx.r.Rename("a/f", ".") },
		"Symlink":   func() error { return fx.r.Symlink("x", ".") },
		"Mkdir":     func() error { return fx.r.Mkdir(".", 0o755) },
		"WriteFile": func() error { _, err := fx.r.WriteFile(".", strings.NewReader("x"), 0o644); return err },
		"OpenFile":  func() error { return closeFile(fx.r.OpenFile(".", os.O_RDONLY, 0)) },
	} {
		if err := op(); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("%s(.): %v, want ErrInvalid", name, err)
		}
	}
	if _, err := Open(fx.root, Options{Reserved: []string{"../x"}}); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("Open with a reserved ../x: %v, want ErrInvalid", err)
	}
}

// TestReservedPaths covers the guard for the paths fleet prepares: never
// removed, renamed or replaced, never a symlink at one or above one, while
// their content stays writable and what is below a reserved folder is free.
func TestReservedPaths(t *testing.T) {
	base := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(base, ".mnt", "opt", "data"), 0o755))
	must(t, os.MkdirAll(filepath.Join(base, "files"), 0o755))
	must(t, os.WriteFile(filepath.Join(base, "files", ".claude.json"), []byte("{}"), 0o644))
	r := openTest(t, base, Options{Reserved: []string{".mnt/opt/data", "files/.claude.json"}}, true)

	refused := map[string]func() error{
		"Remove reserved file":     func() error { return r.Remove("files/.claude.json") },
		"RemoveAll above reserved": func() error { return r.RemoveAll("files") },
		"RemoveAll reserved":       func() error { return r.RemoveAll(".mnt/opt/data") },
		"RemoveAll top above":      func() error { return r.RemoveAll(".mnt") },
		"Rename reserved away":     func() error { return r.Rename(".mnt/opt/data", "elsewhere") },
		"Rename above away":        func() error { return r.Rename(".mnt/opt", "elsewhere") },
		"Rename over reserved":     func() error { return r.Rename("x", "files/.claude.json") },
		"Symlink at reserved":      func() error { return r.Symlink("/etc", ".mnt/opt/data") },
		"Symlink above reserved":   func() error { return r.Symlink("/etc", ".mnt/opt") },
		"WriteFile above reserved": func() error {
			_, err := r.WriteFile(".mnt/opt", strings.NewReader("x"), 0o644)
			return err
		},
		"create above reserved": func() error {
			return closeFile(r.OpenFile(".mnt", os.O_WRONLY|os.O_CREATE, 0o644))
		},
	}
	for name, op := range refused {
		if err := op(); !errors.Is(err, ErrReserved) {
			t.Errorf("%s: %v, want ErrReserved", name, err)
		}
	}
	if _, err := r.WriteFile("files/.claude.json", strings.NewReader(`{"a":1}`), 0o666); err != nil {
		t.Errorf("WriteFile of a reserved file's content: %v", err)
	}
	must(t, r.Symlink("/etc", ".mnt/opt/data2"))
	must(t, r.Symlink("/etc", ".mnt/opt/data/below"))
	must(t, r.Remove(".mnt/opt/data/below"))

	sub, err := r.OpenRoot(".mnt")
	must(t, err)
	defer sub.Close()
	if err := sub.RemoveAll("opt/data"); !errors.Is(err, ErrReserved) {
		t.Errorf("sub-root RemoveAll(opt/data): %v, want ErrReserved", err)
	}
	if sub.Name() != filepath.Join(base, ".mnt") {
		t.Errorf("sub-root Name() = %q", sub.Name())
	}
}

// TestReservedPathsByIdentity checks the guard knows a reserved entry by what
// it is, not only by how it is spelled: a hard link to a reserved file is the
// file, and where the filesystem ignores case (macOS by default) so is any
// spelling of a reserved path.
func TestReservedPathsByIdentity(t *testing.T) {
	base := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(base, ".mnt", "opt", "data"), 0o755))
	must(t, os.WriteFile(filepath.Join(base, ".mnt", "opt", "data", "keep"), []byte("keep"), 0o644))
	must(t, os.MkdirAll(filepath.Join(base, "files"), 0o755))
	must(t, os.WriteFile(filepath.Join(base, "files", ".claude.json"), []byte("{}"), 0o644))
	must(t, os.Link(filepath.Join(base, "files", ".claude.json"), filepath.Join(base, "files", "alias")))
	must(t, os.WriteFile(filepath.Join(base, "x"), []byte("x"), 0o644))
	r := openTest(t, base, Options{Reserved: []string{".mnt/opt/data", "files/.claude.json"}}, true)

	refused := map[string]func() error{
		"Remove the link":      func() error { return r.Remove("files/alias") },
		"RemoveAll the link":   func() error { return r.RemoveAll("files/alias") },
		"Rename the link away": func() error { return r.Rename("files/alias", "moved") },
		"Rename over the link": func() error { return r.Rename("x", "files/alias") },
	}
	if caseInsensitive(t, base) {
		for name, other := range map[string]string{"opt/data": ".mnt/OPT/Data", "opt": ".MNT/Opt", "the file": "files/.Claude.JSON"} {
			refused["RemoveAll "+name+" as "+other] = func() error { return r.RemoveAll(other) }
			refused["Rename "+name+" away as "+other] = func() error { return r.Rename(other, "moved") }
			refused["Rename over "+name+" as "+other] = func() error { return r.Rename("x", other) }
		}
	} else {
		t.Log("case-sensitive filesystem: only the hard link is checked")
	}
	for name, op := range refused {
		if err := op(); !errors.Is(err, ErrReserved) {
			t.Errorf("%s: %v, want ErrReserved", name, err)
		}
	}
	assertFile(t, filepath.Join(base, ".mnt", "opt", "data", "keep"), "keep", 0o644)
	assertFile(t, filepath.Join(base, "files", ".claude.json"), "{}", 0o644)
	must(t, r.Remove("x"))
	must(t, r.RemoveAll("files/missing"))
}

// caseInsensitive reports whether the folder dir ignores case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	must(t, os.WriteFile(probe, nil, 0o644))
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// TestOpenRootFollowsTheRootItself pins the boundary: the root folder belongs
// to the caller and may be a symlink; Open follows it.
func TestOpenRootFollowsTheRootItself(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	must(t, os.Mkdir(real, 0o755))
	must(t, os.Symlink(real, filepath.Join(base, "link")))
	eachWalk(t, func(t *testing.T, portable bool) {
		r := openTest(t, filepath.Join(base, "link"), Options{}, portable)
		_, err := r.WriteFile("x", strings.NewReader("x"), 0o644)
		must(t, err)
		assertFile(t, filepath.Join(real, "x"), "x", 0o644)
		must(t, r.Remove("x"))
	})
}

// TestChangedEntryIsRefused drives the identity check directly: a folder
// whose open does not match the Lstat it was chosen by is refused.
func TestChangedEntryIsRefused(t *testing.T) {
	fx := newFixture(t, true)
	other, err := fx.r.Lstat("inside")
	must(t, err)
	d := fx.r.top()
	if _, err := d.openChildAs("a", other); !errors.Is(err, ErrChanged) {
		t.Errorf("openChildAs with another folder's Lstat: %v, want ErrChanged", err)
	}
}

// TestCloseReleasesTheRoot checks operations fail once the Root is closed.
func TestCloseReleasesTheRoot(t *testing.T) {
	fx := newFixture(t, false)
	if _, err := fx.r.Lstat("a/f"); err != nil {
		t.Fatal(err)
	}
	must(t, fx.r.Close())
	if _, err := fx.r.Lstat("a/f"); err == nil {
		t.Error("Lstat after Close succeeded")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken source") }

func assertFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Errorf("%s = %q, %v; want %q", path, data, err, content)
	}
	assertMode(t, path, mode)
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Errorf("lstat %s: %v", path, err)
		return
	}
	if got := info.Mode()&^fs.ModeType | info.Mode()&fs.ModeDir; got != want {
		t.Errorf("%s mode = %v, want %v", path, got, want)
	}
}

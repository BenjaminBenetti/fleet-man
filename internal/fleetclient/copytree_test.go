package fleetclient

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestTarTreeRoundTrip builds a tar from a directory and extracts it, asserting
// the tree (files, modes, dotfiles, empty dirs) round-trips and symlinks are
// skipped on both ends.
func TestTarTreeRoundTrip(t *testing.T) {
	src := t.TempDir()
	write := func(rel string, mode os.FileMode, content string) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("file.txt", 0o644, "hello")
	write("exec.sh", 0o755, "#!/bin/sh\n")
	write(".dotfile", 0o600, "secret")
	write("sub/nested.txt", 0o644, "nested")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if _, err := writeTarTreeFromWalk(tw, src); err != nil {
		t.Fatalf("writeTarTreeFromWalk: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if _, err := extractTarTree(&buf, dst); err != nil {
		t.Fatalf("extractTarTree: %v", err)
	}

	check := func(rel, content string, mode os.FileMode) {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != content {
			t.Errorf("%s = %q, want %q", rel, got, content)
		}
		fi, _ := os.Stat(filepath.Join(dst, rel))
		if fi.Mode().Perm() != mode {
			t.Errorf("%s mode = %o, want %o", rel, fi.Mode().Perm(), mode)
		}
	}
	check("file.txt", "hello", 0o644)
	check("exec.sh", "#!/bin/sh\n", 0o755)
	check(".dotfile", "secret", 0o600)
	check("sub/nested.txt", "nested", 0o644)
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty dir not recreated: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Errorf("symlink should have been skipped, got %v", err)
	}
}

// TestExtractTarTreeRejectsTraversal feeds a hand-built malicious archive and
// asserts no file lands outside targetRoot (the security contract — the server
// fully controls the tar bytes over the gateway).
func TestExtractTarTreeRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	targetRoot := filepath.Join(root, "target")
	outside := filepath.Join(root, "OUTSIDE")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := []struct {
		name string
		typ  byte
		body string
	}{
		{"../OUTSIDE/escaped.txt", tar.TypeReg, "pwn"},     // parent traversal
		{"/etc/whatever", tar.TypeReg, "pwn"},              // absolute
		{"sub/../../OUTSIDE/escaped2.txt", tar.TypeReg, "pwn"}, // mid traversal
		{"evil", tar.TypeSymlink, ""},                      // symlink (skipped)
		{"ok.txt", tar.TypeReg, "fine"},                    // legit
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			hdr.Linkname = outside
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()

	if _, err := extractTarTree(&buf, targetRoot); err != nil {
		t.Fatalf("extractTarTree: %v", err)
	}
	// Nothing escaped into OUTSIDE.
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("files escaped targetRoot into OUTSIDE: %v", ents)
	}
	// The legit file landed inside; confined traversal names land inside too.
	if _, err := os.Stat(filepath.Join(targetRoot, "ok.txt")); err != nil {
		t.Errorf("legit entry not written: %v", err)
	}
}

// TestExtractTarTreeRejectsSymlinkedParent confirms a write can't follow a
// pre-existing on-disk symlink at an intermediate path component out of the
// target (the in-place-merge escape os.Root closes).
func TestExtractTarTreeRejectsSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	targetRoot := filepath.Join(base, "target")
	evil := filepath.Join(base, "EVIL")
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-plant a symlink inside targetRoot pointing outside it (a merge target
	// could already contain this).
	if err := os.Symlink(evil, filepath.Join(targetRoot, "sub")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "sub/pwned.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("pwn"))
	tw.Close()

	// The extract must fail (escape refused), and nothing lands in EVIL.
	if _, err := extractTarTree(&buf, targetRoot); err == nil {
		t.Fatal("expected an error extracting through a symlinked parent")
	}
	if _, err := os.Stat(filepath.Join(evil, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("write escaped through the symlink into EVIL: %v", err)
	}
}

// TestExtractTarTreeSkipsRootEntry confirms the `./` root entry never rewrites an
// existing targetRoot's mode (a merge must not mutate the destination dir).
func TestExtractTarTreeSkipsRootEntry(t *testing.T) {
	targetRoot := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(targetRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// `tar -C dir -cf - .` emits the root as "./" with the source dir's mode.
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700})
	_ = tw.WriteHeader(&tar.Header{Name: "./inner.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	tw.Close()

	if _, err := extractTarTree(&buf, targetRoot); err != nil {
		t.Fatalf("extractTarTree: %v", err)
	}
	if fi, _ := os.Stat(targetRoot); fi.Mode().Perm() != 0o755 {
		t.Errorf("targetRoot mode changed to %o, want 0755 (root entry must be skipped)", fi.Mode().Perm())
	}
	if got, _ := os.ReadFile(filepath.Join(targetRoot, "inner.txt")); string(got) != "hi" {
		t.Errorf("inner.txt = %q, want hi", got)
	}
}

func TestTarSlashName(t *testing.T) {
	cases := []struct{ in, want string }{
		{".", ""},
		{"./", ""},
		{"./sub/file", "sub/file"},
		{"sub/file", "sub/file"},
		{"../escape", "escape"},
		{"../../etc/passwd", "etc/passwd"},
		{"/abs/path", "abs/path"},
		{"a/b/../../../c", "c"},
		{`..\windows\escape`, "windows/escape"},
	}
	for _, tc := range cases {
		if got := tarSlashName(tc.in); got != tc.want {
			t.Errorf("tarSlashName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFilterTarTree confirms the relay transform drops symlinks/specials and the
// root, and re-emits clean root-relative names.
func TestFilterTarTree(t *testing.T) {
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "./keep.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("keep"))
	_ = tw.WriteHeader(&tar.Header{Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.WriteHeader(&tar.Header{Name: "./sub/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.Close()

	var out bytes.Buffer
	otw := tar.NewWriter(&out)
	if _, err := filterTarTree(otw, tar.NewReader(&in)); err != nil {
		t.Fatalf("filterTarTree: %v", err)
	}
	otw.Close()

	var names []string
	tr := tar.NewReader(&out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	want := []string{"keep.txt", "sub/"}
	if len(names) != len(want) {
		t.Fatalf("filtered names = %v, want %v (root + symlink dropped)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("name[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestCopyTreeLocal exercises the local→local recursive copy.
func TestCopyTreeLocal(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "d/b.txt"), []byte("bbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "sl")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "copy")
	written, err := copyTreeLocal(src, dst)
	if err != nil {
		t.Fatalf("copyTreeLocal: %v", err)
	}
	if written != 6 {
		t.Errorf("written = %d, want 6", written)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "d/b.txt")); string(got) != "bbb" {
		t.Errorf("d/b.txt = %q, want bbb", got)
	}
	if _, err := os.Lstat(filepath.Join(dst, "sl")); !os.IsNotExist(err) {
		t.Errorf("symlink should be skipped, got %v", err)
	}
}

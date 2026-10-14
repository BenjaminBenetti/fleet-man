//go:build linux

package podseed

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A share's tree is copied whole onto the volume, replacing what it held
// (its lost+found kept): files with their modes, empty and nested folders,
// symlinks as links (never followed), hard links as links, sockets; then
// checked against the share.
func TestCopyShare(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write := func(p, data string, mode os.FileMode) {
		t.Helper()
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/.credentials.json", `{"token":"x"}`, 0o600)
	write(".claude.json", `{"a":1}`, 0o644)
	write(".mnt/opt/data/big.bin", strings.Repeat("x", 1<<20), 0o640)
	write("sync/state/rev", "42", 0o600)
	if err := os.MkdirAll(filepath.Join(src, "empty/nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../etc/passwd", filepath.Join(src, ".claude/escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, ".claude.json"), filepath.Join(src, ".claude.json.bak")); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(src, "sync/spoke.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.MkdirAll(filepath.Join(src, shareCopyLostFound), 0o700); err != nil {
		t.Fatal(err)
	}
	// What the volume held before: replaced, but for its lost+found.
	for _, p := range []string{"stale/file", shareCopyLostFound + "/kept"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dst, p)), 0o700)
		if err := os.WriteFile(filepath.Join(dst, p), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var stats copyStats
	res, err := copyShare(context.Background(), src, dst, 2, &stats, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 5 || res.Bytes != int64(1<<20+len(`{"token":"x"}`)+2*len(`{"a":1}`)+2) {
		t.Fatalf("copied %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Fatalf("what the volume held stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, shareCopyLostFound, "kept")); err != nil {
		t.Fatalf("lost+found was not kept: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(dst, ".claude/escape")); err != nil || got != "../../etc/passwd" {
		t.Fatalf("symlink %q %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, ".claude/.credentials.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials %v %v", fi, err)
	}
	a, _ := os.Stat(filepath.Join(dst, ".claude.json"))
	b, _ := os.Stat(filepath.Join(dst, ".claude.json.bak"))
	if !os.SameFile(a, b) {
		t.Fatal("the hard link became two files")
	}
	if fi, err := os.Lstat(filepath.Join(dst, "sync/spoke.sock")); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "empty/nested")); err != nil || !fi.IsDir() {
		t.Fatalf("nested folder %v %v", fi, err)
	}
}

// The check after a copy names what differs from the share.
func TestCompareShareNamesWhatDiffers(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, d := range []string{src, dst} {
		if err := os.WriteFile(filepath.Join(d, "same"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "gone"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "mode"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "mode"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "extra"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := compareShare(src, dst, false)
	for _, want := range []string{"gone: missing from the copy", "mode: mode 0644, the share's 0600", "extra: in the copy only"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err %v lacks %q", err, want)
		}
	}
}

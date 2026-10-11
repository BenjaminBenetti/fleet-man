//go:build linux

package podseed

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		mustMkdir(t, filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func syncPass(t *testing.T, s *syncer, tops ...string) int64 {
	t.Helper()
	n, err := s.pass(context.Background(), tops)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSyncPassesWriteOnlyWhatChanged(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"ws/a": "a", "ws/b": "b", "ws/sub/c": "c"})
	var stats copyStats
	s, err := newSyncer(src, dst, true, 2, &stats)
	if err != nil {
		t.Fatal(err)
	}
	if n := syncPass(t, s, "ws"); n < 4 {
		t.Fatalf("the first pass wrote %d entries", n)
	}
	if n := syncPass(t, s, "ws"); n != 0 {
		t.Fatalf("a pass over an unchanged tree wrote %d entries", n)
	}
	// A chmod moves only the change time.
	if err := os.Chmod(filepath.Join(src, "ws/a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := syncPass(t, s, "ws"); n != 1 {
		t.Fatalf("a chmod made %d writes, want 1", n)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "ws/a")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// A file becomes a dir, a dir goes.
	if err := os.Remove(filepath.Join(src, "ws/b")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, src, map[string]string{"ws/b/inner": "now a dir"})
	if err := os.RemoveAll(filepath.Join(src, "ws/sub")); err != nil {
		t.Fatal(err)
	}
	syncPass(t, s, "ws")
	if got, _ := os.ReadFile(filepath.Join(dst, "ws/b/inner")); string(got) != "now a dir" {
		t.Fatalf("ws/b/inner %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dst, "ws/sub")); !os.IsNotExist(err) {
		t.Fatalf("a deleted dir stayed: %v", err)
	}
}

func TestSyncKeepsHardLinksAcrossPasses(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"r/one": "shared"})
	if err := os.Link(filepath.Join(src, "r/one"), filepath.Join(src, "r/two")); err != nil {
		t.Fatal(err)
	}
	var stats copyStats
	s, _ := newSyncer(src, dst, false, 1, &stats)
	syncPass(t, s, "r")
	syncPass(t, s, "r")
	// Rewritten through one name: both copies follow.
	if err := os.WriteFile(filepath.Join(src, "r/two"), []byte("rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncPass(t, s, "r")
	a, b := lstat(t, filepath.Join(dst, "r/one")), lstat(t, filepath.Join(dst, "r/two"))
	if a.Ino != b.Ino {
		t.Fatal("the link broke into two files")
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "r/one")); string(got) != "rewritten" {
		t.Fatalf("r/one %q", got)
	}
}

func TestSyncLeavesExcludedPathsAlone(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"fleet/bin/x": "bin", "fleet/log/pid1.log": "source log"})
	writeTree(t, dst, map[string]string{"fleet/log/own.log": "kept"})
	var stats copyStats
	s, _ := newSyncer(src, dst, false, 1, &stats)
	s.exclude = cloneExcluded
	syncPass(t, s, "fleet")
	if _, err := os.Lstat(filepath.Join(dst, "fleet/log/pid1.log")); !os.IsNotExist(err) {
		t.Fatal("the source's log was copied")
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "fleet/log/own.log")); string(got) != "kept" {
		t.Fatal("an excluded path on the clone was deleted")
	}
}

func TestALiveCopyLeavesGitLocksOut(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{
		"ws/.git/HEAD": "ref: refs/heads/main\n", "ws/.git/index.lock": "held",
		"ws/.git/refs/heads/main.lock": "held", "ws/.git/objects/pack/tmp_pack_x": "partial",
		"ws/.git/objects/ab/cdef": "object", "ws/index.lock": "not git's",
	})
	var stats copyStats
	s, _ := newSyncer(src, dst, true, 1, &stats)
	syncPass(t, s, "ws")
	for _, p := range []string{"ws/.git/index.lock", "ws/.git/refs/heads/main.lock", "ws/.git/objects/pack/tmp_pack_x"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s was copied live", p)
		}
	}
	for _, p := range []string{"ws/.git/HEAD", "ws/.git/objects/ab/cdef", "ws/index.lock"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	// A copy of a source held still keeps them.
	still, _ := newSyncer(src, t.TempDir(), false, 1, &stats)
	syncPass(t, still, "ws")
	if _, err := os.Lstat(filepath.Join(still.dst, "ws/.git/index.lock")); err != nil {
		t.Errorf("a still copy dropped the lock: %v", err)
	}
}

func TestGitDirsAreReadRefsFirst(t *testing.T) {
	git := []string{"HEAD", "config", "index", "objects", "packed-refs", "refs"}
	if got := orderEntries("/x/.git", git, true); !reflect.DeepEqual(got, []string{"HEAD", "config", "index", "packed-refs", "refs", "objects"}) {
		t.Errorf("git dir order %v", got)
	}
	pack := []string{"pack-1.idx", "pack-1.pack", "pack-1.rev", "pack-2.idx", "pack-2.pack"}
	if got := orderEntries("/x/.git/objects/pack", pack, false); !reflect.DeepEqual(got, []string{"pack-1.pack", "pack-2.pack", "pack-1.rev", "pack-1.idx", "pack-2.idx"}) {
		t.Errorf("pack order %v", got)
	}
	if got := orderEntries("/x/src", []string{"objects", "a"}, false); !reflect.DeepEqual(got, []string{"objects", "a"}) {
		t.Errorf("a plain dir reordered: %v", got)
	}
	if !isGitDir("/x/repo.git", []string{"HEAD", "objects", "refs", "config"}) || isGitDir("/x/src", []string{"HEAD"}) {
		t.Error("isGitDir")
	}
}

func TestSyncCopiesAFileThatChangedWhileItWasReadAgain(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"ws/f": "v1"})
	var stats copyStats
	s, _ := newSyncer(src, dst, true, 1, &stats)
	syncPass(t, s, "ws")
	// As if the file had changed after the walk read it: no change time is
	// kept for it, so the next pass copies it whatever its stat says.
	s.seenMu.Lock()
	delete(s.seen, "ws/f")
	s.seenMu.Unlock()
	if n := syncPass(t, s, "ws"); n != 1 {
		t.Fatalf("a file whose copy was not trusted made %d writes", n)
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(dst, "ws/f"), &st); err != nil {
		t.Fatal(err)
	}
}

//go:build unix

package sharesync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoteHintScansThePath: a spoke that watches nothing (and rescans
// once an hour) reports a file a pod on another node wrote as soon as the
// hub hints it; a hint for a root or path the spoke does not have changes
// nothing.
func TestRemoteHintScansThePath(t *testing.T) {
	r := newRig(t, "host")
	hints := make(chan Hint, 4)
	rs := addRemote(t, r, SideInfo{ID: "cluster:share", Location: "cluster"}, RemoteOptions{Hints: hints})
	waitAttached(t, r, "cluster:share")

	rs.write(t, "tool", "projects/a/new.jsonl", "from another node")
	hints <- Hint{Root: "nope", Paths: []string{"projects/a/new.jsonl"}}
	hints <- Hint{Root: "tool", Paths: []string{"../escape", "/abs"}}
	time.Sleep(200 * time.Millisecond)
	if got := r.read("host", "tool", "projects/a/new.jsonl"); got != "<missing>" {
		t.Fatalf("a hint the spoke does not have scanned anyway: the host has %q", got)
	}
	hints <- Hint{Root: "tool", Paths: []string{"projects/a/new.jsonl"}}
	waitFor(t, 5*time.Second, func() bool { return r.read("host", "tool", "projects/a/new.jsonl") == "from another node" }, r)

	// "." is the whole root.
	rs.write(t, "user", "deep/x/y.txt", "y")
	hints <- Hint{Root: "user", Paths: []string{"."}}
	waitFor(t, 5*time.Second, func() bool { return r.read("host", "user", "deep/x/y.txt") == "y" }, r)
}

// TestSideHintCapsItsPaths: a hint marks its paths for the next scan; one
// of more paths than a side takes marks the whole root instead, and bad
// paths are dropped.
func TestSideHintCapsItsPaths(t *testing.T) {
	r := newRig(t, "host")
	// A side of its own over the host's tree that is not running: nothing
	// scans what the hints mark.
	s := NewSide(SideInfo{ID: "idle"}, r.sides["host"].tree, nil, SideOptions{})
	s.Hint("tool", []string{"a/b.txt", "../up", "c"})
	if got := s.dirty["tool"]; len(got) != 2 || !got["a/b.txt"] || !got["c"] {
		t.Fatalf("a hint marked %v", got)
	}
	var paths []string
	for i := 0; i <= hintPaths; i++ {
		paths = append(paths, fmt.Sprintf("p%d", i))
	}
	s.Hint("user", paths)
	if got := s.dirty["user"]; len(got) != 1 || !got["."] {
		t.Fatalf("a hint of %d paths marked %v", len(paths), got)
	}
	s.Hint("nope", []string{"x"})
	if _, ok := s.dirty["nope"]; ok {
		t.Fatal("a hint for a root the side does not have was marked")
	}
}

// TestWatchReportsChanges: the exported watcher reports a change below its
// folder, and leaves a skipped folder alone.
func TestWatchReportsChanges(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"a", "skip"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w, err := Watch(dir, func(rel string) bool { return rel == "skip" }, nil)
	if err != nil {
		t.Skipf("no watcher here: %v", err)
	}
	defer w.Close()
	if err := os.WriteFile(filepath.Join(dir, "skip", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	deadline := time.After(5 * time.Second)
	for !containsPath(got, "a/f") {
		select {
		case <-w.Ready():
			got = append(got, w.Drain()...)
		case <-deadline:
			t.Fatalf("the watcher reported %v, not a/f", got)
		}
	}
	if containsPath(got, "skip/x") {
		t.Errorf("a skipped folder's change was reported: %v", got)
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// waitFor polls cond until it holds, failing with the rig's dump after d.
func waitFor(t *testing.T, d time.Duration, cond func() bool, r *rig) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out\n%s", r.dump())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

//go:build linux

package sharesync

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// waitPaths drains w until it has reported every path in want, or fails.
func waitPaths(t *testing.T, w watcher, want ...string) []string {
	t.Helper()
	var seen []string
	deadline := time.After(5 * time.Second)
	for {
		missing := false
		for _, p := range want {
			if !slices.Contains(seen, p) && !slices.Contains(seen, ".") {
				missing = true
			}
		}
		if !missing {
			return seen
		}
		select {
		case <-w.ready():
			seen = append(seen, w.drain()...)
		case <-deadline:
			t.Fatalf("watcher reported %q, want %q", seen, want)
		}
	}
}

// TestInotifyWatcherReportsChanges checks the watcher reports files made,
// changed and removed, in folders made after it started too, and never
// watches a folder skip names.
func TestInotifyWatcherReportsChanges(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "downloads"), 0o755)
	w, err := watchDir(dir, func(rel string) bool { return rel == "downloads" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("watchDir: %v", err)
	}
	defer w.close()
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	waitPaths(t, w, "f")

	os.MkdirAll(filepath.Join(dir, "new", "deeper"), 0o755)
	waitPaths(t, w, "new")
	// The new folders get watches of their own (a file made before that is
	// the scan of "new"'s to find): a file made in them is reported.
	iw := w.(*inotifyWatcher)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		iw.mu.Lock()
		watched := slices.Contains(mapValues(iw.wds), "new/deeper")
		iw.mu.Unlock()
		if watched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new/deeper was never watched")
		}
	}
	os.WriteFile(filepath.Join(dir, "new", "deeper", "g"), []byte("x"), 0o644)
	waitPaths(t, w, "new/deeper/g")

	os.Remove(filepath.Join(dir, "f"))
	waitPaths(t, w, "f")

	os.WriteFile(filepath.Join(dir, "downloads", "big"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644)
	seen := waitPaths(t, w, "marker")
	if slices.Contains(seen, "downloads/big") {
		t.Errorf("a skipped folder was watched: %q", seen)
	}
	if w.limited() {
		t.Error("watcher reports the watch limit")
	}
}

// TestSideRunFollowsTheWatcher checks a running side picks up a change from
// its watcher alone (no flush, no poll) and the hub delivers it.
func TestSideRunFollowsTheWatcher(t *testing.T) {
	r := newRig(t, "c1")
	host := &rigSide{info: SideInfo{ID: "host", Location: "host"}, dir: filepath.Join(r.base, "sides", "host"), reserved: map[string][]string{}}
	for _, root := range r.roots {
		os.MkdirAll(filepath.Join(host.dir, root), 0o755)
	}
	var roots []TreeRoot
	for _, root := range r.roots {
		roots = append(roots, TreeRoot{Name: root, Dir: filepath.Join(host.dir, root)})
	}
	tree, err := OpenTree(roots, TreeOptions{StateDir: filepath.Join(r.base, "state", "host"), Table: r.table, Settle: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	side := NewSide(host.info, tree, r.hub, SideOptions{Poll: time.Hour, Rescan: time.Hour, Debounce: 5 * time.Millisecond})
	if err := side.Attach(context.Background(), r.hub); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); side.Run(ctx) }()
	defer func() { cancel(); <-done; r.hub.Detach("host") }()

	time.Sleep(50 * time.Millisecond) // the first full scan and the watches
	os.WriteFile(filepath.Join(host.dir, "user", "seen"), []byte("by inotify"), 0o644)
	deadline := time.Now().Add(5 * time.Second)
	for r.read("c1", "user", "seen") != "by inotify" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.read("c1", "user", "seen"); got != "by inotify" {
		t.Errorf("c1 user/seen = %q: the watcher's change never arrived", got)
	}
}

func mapValues(m map[int32]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

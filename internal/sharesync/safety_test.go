//go:build unix

package sharesync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestSymlinkSwapNeverEscapes plays a docker instance that keeps swapping a
// folder of the host tree for a symlink to a folder outside it while the hub
// applies a pod's files into that folder: nothing is ever written outside,
// and once the swapping stops the sides converge.
func TestSymlinkSwapNeverEscapes(t *testing.T) {
	r := newRig(t, "host", "c1")
	outside := filepath.Join(r.base, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600)
	r.write("host", "user", "d/seed", "seed")
	r.sync()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d := r.path("host", "user", "d")
		aside := r.path("host", "user", ".d-aside")
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Swap the folder for a symlink to outside, then back.
			if os.Rename(d, aside) == nil {
				os.Symlink(outside, d)
				time.Sleep(50 * time.Microsecond)
				os.Remove(d)
				os.Rename(aside, d)
			}
		}
	}()
	for i := range 40 {
		r.write("c1", "user", fmt.Sprintf("d/f%02d", i), fmt.Sprintf("pod file %d", i))
		r.flushErr(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 || entries[0].Name() != "secret" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("sync wrote outside the host tree: %v", names)
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "secret")); string(data) != "secret" {
		t.Fatalf("outside/secret = %q", data)
	}
	r.sync()
	r.sync()
	r.assertConverged()
}

// TestReservedPathsAreNeverReplaced checks a reserved path (a Layout file) on
// the host takes new content but is never deleted or replaced by a symlink,
// whatever a pod does to its copy: the side refuses, the hub shows it.
func TestReservedPathsAreNeverReplaced(t *testing.T) {
	r := newRig(t)
	host := &rigSide{info: SideInfo{ID: "host", Location: "host"}, dir: filepath.Join(r.base, "sides", "host"),
		reserved: map[string][]string{"tool": {"settings.json"}}}
	for _, root := range r.roots {
		os.MkdirAll(filepath.Join(host.dir, root), 0o755)
	}
	os.WriteFile(filepath.Join(host.dir, "tool", "settings.json"), []byte("{}"), 0o644)
	r.sides["host"], r.order = host, append(r.order, "host")
	r.openSide(host)
	r.addSide(SideInfo{ID: "c1", Location: "k1"})
	r.sync()

	r.write("c1", "tool", "settings.json", `{"edited":true}`)
	r.sync()
	if got := r.read("host", "tool", "settings.json"); got != `{"edited":true}` {
		t.Errorf("a reserved file's content was not taken: %q", got)
	}
	os.Remove(r.path("c1", "tool", "settings.json"))
	os.Symlink("/etc/passwd", r.path("c1", "tool", "settings.json"))
	// The refusal is final until the path changes again: nothing is left
	// pending.
	if err := r.flushErr(time.Second); err != nil {
		t.Errorf("Flush after the refusal: %v", err)
	}
	info, err := os.Lstat(r.path("host", "tool", "settings.json"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the reserved file was replaced: %v, %v", info, err)
	}
	var refused []string
	for _, root := range r.hub.Status().Roots {
		for _, side := range root.Sides {
			refused = append(refused, side.Refused...)
		}
	}
	if len(refused) != 1 || !strings.HasPrefix(refused[0], "settings.json: ") {
		t.Errorf("Refused = %q, want settings.json", refused)
	}
}

// TestSetIDBitsNeverLand checks a set-user-ID file made in a pod reaches the
// host without the bit.
func TestSetIDBitsNeverLand(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("c1", "user", "tool", "#!/bin/sh\n")
	if err := os.Chmod(r.path("c1", "user", "tool"), 0o4755|fs.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(r.path("c1", "user", "tool")); info.Mode()&fs.ModeSetuid == 0 {
		t.Skip("this filesystem drops set-ID bits itself")
	}
	r.sync()
	info, err := os.Stat(r.path("host", "user", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 || info.Mode().Perm() != 0o755 {
		t.Errorf("host user/tool mode %v, want 0755 without set-ID", info.Mode())
	}
}

// TestSpecialFilesAreNeverSynced checks a FIFO or socket is never sent, and
// never taken for a delete of the file it replaced.
func TestSpecialFilesAreNeverSynced(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "p", "a file")
	r.sync()
	os.Remove(r.path("c1", "user", "p"))
	if err := syscall.Mkfifo(r.path("c1", "user", "p"), 0o644); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	syscall.Mkfifo(r.path("c1", "user", "q"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.hub.Flush(ctx)
	if got := r.read("host", "user", "p"); got != "a file" {
		t.Errorf("host user/p = %q, want it kept", got)
	}
	if _, err := os.Lstat(r.path("host", "user", "q")); !os.IsNotExist(err) {
		t.Errorf("a FIFO reached the host: %v", err)
	}
}

// TestUnreadableFolderIsNotADelete checks a folder a scan cannot read never
// makes what was in it look deleted, and is warned of once and left to the
// periodic rescan rather than scanned again and again (a root-owned 0700
// folder in a custom mount would otherwise flood the daemon's log).
func TestUnreadableFolderIsNotADelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	r := newRig(t, "host", "c1")
	r.write("host", "user", "d/f", "keep me")
	r.sync()
	d := r.path("c1", "user", "d")
	os.Chmod(d, 0)
	defer os.Chmod(d, 0o755)
	r.flushErr(500 * time.Millisecond)
	if got := r.read("host", "user", "d/f"); got != "keep me" {
		t.Errorf("host user/d/f = %q after c1 could not read d", got)
	}
	for range 3 {
		if _, again, err := r.sides["c1"].tree.Scan(context.Background(), "user", nil); err != nil || len(again) != 0 {
			t.Errorf("scan with d unreadable: again %q, %v; want none", again, err)
		}
	}
	if n := strings.Count(r.logs.String(), "cannot scan"); n != 1 {
		t.Errorf("%d warnings for the unreadable folder, want 1", n)
	}
	// Readable again, and unreadable once more: warned of again.
	os.Chmod(d, 0o755)
	r.sides["c1"].tree.Scan(context.Background(), "user", nil)
	os.Chmod(d, 0)
	r.sides["c1"].tree.Scan(context.Background(), "user", nil)
	if n := strings.Count(r.logs.String(), "cannot scan"); n != 2 {
		t.Errorf("%d warnings after d came back and went again, want 2", n)
	}
}

// TestAFolderRefusingThePathsBelowIsWarnedOfOnce checks a folder sync cannot
// read is warned of once, by its own name, whether a scan reaches it whole
// or through a path below it (a rescan of one file there).
func TestAFolderRefusingThePathsBelowIsWarnedOfOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	r := newRig(t, "host", "c1")
	r.write("host", "user", "d/f", "keep me")
	r.sync()
	r.sides["c1"].stop()
	tree := r.sides["c1"].tree
	d := r.path("c1", "user", "d")
	os.Chmod(d, 0)
	defer os.Chmod(d, 0o755)
	tree.Scan(context.Background(), "user", []string{"d/f"})
	tree.Scan(context.Background(), "user", nil)
	logs := r.logs.String()
	if n := strings.Count(logs, "cannot scan"); n != 1 || !strings.Contains(logs, "path=d ") {
		t.Errorf("%d warnings for the unreadable folder, want 1 naming d:\n%s", n, logs)
	}
}

// TestUnreadableFileIsWarnedOfOnce checks a file sync cannot read (a root-
// owned 0600 file a docker instance left in a custom mount) is warned of
// once, left to the periodic rescan rather than read again and again, and
// never taken for deleted; it syncs once it can be read.
func TestUnreadableFileIsWarnedOfOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	r := newRig(t, "host", "c1")
	r.write("host", "user", "key.pem", "v1")
	r.sync()
	p := r.path("host", "user", "key.pem")
	r.write("host", "user", "key.pem", "v2")
	os.Chmod(p, 0)
	defer os.Chmod(p, 0o644)
	for range 3 {
		rep, again, err := r.sides["host"].tree.Scan(context.Background(), "user", nil)
		if err != nil || len(again) != 0 || len(rep.Changes) != 0 {
			t.Errorf("scan with key.pem unreadable: %+v, again %q, %v; want nothing", rep.Changes, again, err)
		}
	}
	if n := strings.Count(r.logs.String(), `msg="sharesync: cannot scan" tree=host root=user path=key.pem`); n != 1 {
		t.Errorf("%d warnings for the unreadable file, want 1", n)
	}
	if got := r.read("c1", "user", "key.pem"); got != "v1" {
		t.Errorf("c1 key.pem = %q while the host's could not be read", got)
	}
	os.Chmod(p, 0o644)
	r.sync()
	if got := r.read("c1", "user", "key.pem"); got != "v2" {
		t.Errorf("c1 key.pem = %q once the host's could be read", got)
	}
}

// TestScanCutShortReportsNoDeletes checks a scan whose context ends mid-walk
// stops there (a full scan of a large tree heeds the shutdown's deadline),
// returns its error and never takes what it did not reach for deleted.
func TestScanCutShortReportsNoDeletes(t *testing.T) {
	r := newRig(t, "host")
	for i := range 50 {
		r.write("host", "user", fmt.Sprintf("d%02d/f", i), "x")
	}
	r.sync()
	r.sides["host"].stop()
	tree := r.sides["host"].tree
	ctx := &cutCtx{Context: context.Background(), left: 10}
	rep, _, err := tree.Scan(ctx, "user", nil)
	if !errors.Is(err, context.Canceled) || len(rep.Changes) != 0 {
		t.Errorf("a scan cut short: %d changes, %v; want none and its error", len(rep.Changes), err)
	}
	if n := ctx.calls.Load(); n > 20 {
		t.Errorf("the walk went on after its context ended: %d checks", n)
	}
	rep, _, err = tree.Scan(context.Background(), "user", nil)
	if err != nil || len(rep.Changes) != 0 {
		t.Errorf("the next scan: %+v, %v; want nothing changed", rep.Changes, err)
	}
}

// cutCtx is a context that ends after left checks of Err.
type cutCtx struct {
	context.Context
	left  int64
	calls atomic.Int64
}

func (c *cutCtx) Err() error {
	if c.calls.Add(1) > c.left {
		return context.Canceled
	}
	return nil
}

// TestTreeRefusesAMissingFolder checks a tree only opens on folders that are
// there (the resolver makes them).
func TestTreeRefusesAMissingFolder(t *testing.T) {
	_, err := OpenTree([]TreeRoot{{Name: "user", Dir: filepath.Join(t.TempDir(), "missing")}}, TreeOptions{StateDir: t.TempDir(), Table: testTable()})
	if err == nil {
		t.Error("OpenTree took a missing folder")
	}
	_, err = OpenTree([]TreeRoot{{Name: "nope", Dir: t.TempDir()}}, TreeOptions{StateDir: t.TempDir(), Table: testTable()})
	if err == nil || !strings.Contains(err.Error(), "no policy root covers nope") {
		t.Errorf("OpenTree of a root the table lacks: %v", err)
	}
}

// TestRootFolderGoneIsNotADeleteOfEverything checks a root folder deleted
// whole (or moved and made again) stops its scans with ErrRootMoved instead
// of reporting everything in it deleted.
func TestRootFolderGoneIsNotADeleteOfEverything(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "a", "keep")
	r.write("host", "user", "d/b", "keep")
	r.sync()
	// Renamed away before the new folder is made, so the new one cannot
	// take the old one's inode number (tmpfs hands a freed one out again).
	root := r.path("c1", "user", ".")
	if err := os.Rename(root, root+".gone"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(root, 0o755)
	os.RemoveAll(root + ".gone")
	_, _, err := r.sides["c1"].tree.Scan(context.Background(), "user", nil)
	if !errors.Is(err, ErrRootMoved) {
		t.Fatalf("Scan of a folder made again: %v, want ErrRootMoved", err)
	}
	r.flushErr(300 * time.Millisecond)
	if got := r.read("host", "user", "d/b"); got != "keep" {
		t.Errorf("host user/d/b = %q: a root folder made again deleted everything", got)
	}
	// Opened again on the new folder, the side gets everything back.
	r.restartSide("c1")
	r.sync()
	r.assertConverged()
}

// onScanStep runs hook at each step of every scan of the root folder dir,
// until the test ends.
func onScanStep(t *testing.T, dir string, hook func(step string)) {
	t.Helper()
	dir = filepath.Clean(dir)
	f := func(at, step string) {
		if at == dir {
			hook(step)
		}
	}
	scanStep.Store(&f)
	t.Cleanup(func() { scanStep.Store(nil) })
}

// TestRootFolderMovedMidScanIsNotADelete checks a root folder moved away and
// made again while a scan walks it (past the scan's first look at the
// folder) stops that scan with ErrRootMoved: what the walk then found
// missing is not reported deleted.
func TestRootFolderMovedMidScanIsNotADelete(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "a", "keep")
	r.write("host", "user", "d/b", "keep")
	r.sync()
	r.sides["c1"].stop()
	root := r.path("c1", "user", ".")
	onScanStep(t, root, func(step string) {
		if step != "walk" {
			return
		}
		if err := os.Rename(root, root+".gone"); err != nil {
			t.Error(err)
		}
		os.MkdirAll(root, 0o755)
		os.RemoveAll(root + ".gone")
	})
	rep, _, err := r.sides["c1"].tree.Scan(context.Background(), "user", nil)
	if !errors.Is(err, ErrRootMoved) || len(rep.Changes) != 0 {
		t.Fatalf("a scan whose folder moved mid-walk: %d changes, %v; want none, ErrRootMoved", len(rep.Changes), err)
	}
}

// TestAnOlderScanKeepsAnUnreadableFolderWarnedOf checks a scan that walked a
// folder while it could still be read, and ends after a later scan found it
// unreadable, does not take the warning back: the next scan does not warn
// of it again.
func TestAnOlderScanKeepsAnUnreadableFolderWarnedOf(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	r := newRig(t, "host", "c1")
	r.write("host", "user", "d/f", "keep me")
	r.sync()
	r.sides["c1"].stop()
	tree := r.sides["c1"].tree
	d := r.path("c1", "user", "d")
	defer os.Chmod(d, 0o755)
	var later atomic.Bool
	onScanStep(t, r.path("c1", "user", "."), func(step string) {
		if step == "walked" && later.CompareAndSwap(false, true) {
			// d unreadable now, and a later scan runs to its end first.
			os.Chmod(d, 0)
			tree.Scan(context.Background(), "user", nil)
		}
	})
	tree.Scan(context.Background(), "user", nil)
	tree.Scan(context.Background(), "user", nil)
	if !later.Load() {
		t.Fatal("the later scan never ran")
	}
	if n := strings.Count(r.logs.String(), "cannot scan"); n != 1 {
		t.Errorf("%d warnings for the unreadable folder, want 1", n)
	}
}

//go:build unix

package sharesync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// TestTombstonesAreForgotten checks a delete every attached side has had is
// dropped from the hub's state after TombstoneTTL, and a side that still
// held the path from before is told to delete it all the same.
func TestTombstonesAreForgotten(t *testing.T) {
	r := newRig(t)
	r.hopts.TombstoneTTL = time.Nanosecond
	r.hub.Close()
	r.openHub()
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "c1", Location: "k1"})
	r.write("host", "user", "gone", "x")
	r.write("host", "user", "kept", "y")
	r.sync()
	// c1 is away while the host deletes; the host alone has the delete.
	r.sides["c1"].stop()
	r.hub.Detach("c1")
	os.Remove(r.path("host", "user", "gone"))
	r.sync()
	r.restartHub()
	root := r.hub.roots["user"]
	root.mu.Lock()
	_, remembered := root.entries[key{path: "gone"}]
	root.mu.Unlock()
	if remembered {
		t.Error("a delete no attached side holds is still remembered")
	}
	r.sync()
	if got := r.read("c1", "user", "gone"); got != "<missing>" {
		t.Errorf("c1 user/gone = %q: the forgotten delete never reached it", got)
	}
	r.assertConverged()
}

// TestStatusWarnsAndCounts checks the size warning of a root part grown
// large, and the pending counts of a side behind.
func TestStatusWarnsAndCounts(t *testing.T) {
	r := &rig{t: t, base: t.TempDir(), roots: []string{"tool"}, sides: map[string]*rigSide{}, logs: &logBuffer{}}
	r.table = syncpolicy.NewTable(syncpolicy.Root{
		Path: "tool", Default: syncpolicy.Mirror,
		Warnings: []syncpolicy.SizeWarning{{Pattern: "projects/**", Size: 10}},
	})
	r.hopts = HubOptions{NormalDelay: time.Millisecond, BulkDelay: time.Millisecond, Logger: r.logs.logger()}
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1} }
	r.openHub()
	t.Cleanup(r.close)
	r.addSide(SideInfo{ID: "host", Location: "host"})
	f := r.faultySide("c1", "k1")
	f.set(func(f *faultyPeer) { f.down = true })
	r.write("host", "tool", "projects/a.jsonl", strings.Repeat("x", 12))
	r.write("host", "tool", "todo", "1")
	r.flushErr(100 * time.Millisecond)
	st := r.hub.Status().Roots[0]
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "tool/projects/** holds 12 B (over 10 B)") {
		t.Errorf("warnings = %q", st.Warnings)
	}
	if st.Files != 2 || st.Bytes != 13 {
		t.Errorf("files %d bytes %d, want 2 and 13", st.Files, st.Bytes)
	}
	var c1 SideStatus
	for _, s := range st.Sides {
		if s.Side == "c1" {
			c1 = s
		}
	}
	if c1.Pending != 3 || c1.PendingBytes != 13 {
		t.Errorf("c1 pending %d (%d bytes), want the folder and two files (13 bytes)", c1.Pending, c1.PendingBytes)
	}
}

// TestBulkLaneWaits checks a large file waits out the bulk delay, a small one
// only the normal delay.
func TestBulkLaneWaits(t *testing.T) {
	r := newRig(t)
	r.hub.Close()
	r.hopts.BulkDelay = 400 * time.Millisecond
	r.openHub()
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "c1", Location: "k1"})
	big := strings.Repeat("b", BulkSize+1)
	r.write("host", "user", "big", big)
	r.write("host", "user", "small", "s")
	start := time.Now()
	if err := r.sides["host"].side.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for r.read("c1", "user", "small") != "s" && time.Since(start) < 5*time.Second {
		time.Sleep(2 * time.Millisecond)
	}
	if got := r.read("c1", "user", "big"); got != "<missing>" && time.Since(start) < 400*time.Millisecond {
		t.Error("the large file skipped the bulk delay")
	}
	for r.read("c1", "user", "big") != big && time.Since(start) < 5*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	if took := time.Since(start); took < 400*time.Millisecond {
		t.Errorf("the large file arrived after %v, before the bulk delay", took)
	}
}

// TestBatchOfAnotherEpoch checks an op of an epoch the tree has not seen
// moves the tree to it and holds back what the tree has: everything it
// holds is reported again, and nothing it holds is replaced unseen.
func TestBatchOfAnotherEpoch(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("c1", "user", "mine", "c1's")
	r.sync()
	tree := r.sides["c1"].tree
	res, err := tree.Apply(context.Background(), Batch{Root: "user", Epoch: "another", Ops: []Op{{
		Path: "mine", Rev: 1, Entry: Entry{Kind: File, Mode: 0o644, Size: 3, Hash: sum("new")},
	}}})
	if err != nil || res[0].Status != Changed {
		t.Fatalf("an op of another epoch over held content: %+v, %v", res, err)
	}
	rep, _, err := tree.Scan(context.Background(), "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Epoch != "another" || len(rep.Changes) != 1 || rep.Changes[0].Path != "mine" || rep.Changes[0].Base != 0 {
		t.Errorf("report after the epoch moved: %+v", rep)
	}
}

// TestSettleWaitsForALargeFile checks a large file modified a moment ago is
// not reported until it has gone unmodified for the settle time.
func TestSettleWaitsForALargeFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "user"), 0o755)
	tree, err := OpenTree([]TreeRoot{{Name: "user", Dir: filepath.Join(dir, "user")}}, TreeOptions{StateDir: t.TempDir(), Table: testTable(), Settle: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	os.WriteFile(filepath.Join(dir, "user", "big"), []byte(strings.Repeat("x", BulkSize+1)), 0o644)
	os.WriteFile(filepath.Join(dir, "user", "small"), []byte("x"), 0o644)
	rep, again, err := tree.Scan(context.Background(), "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Changes) != 1 || rep.Changes[0].Path != "small" || len(again) != 1 || again[0] != "big" {
		t.Errorf("changes %+v, again %q: want small now and big later", rep.Changes, again)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(dir, "user", "big"), old, old)
	rep, _, _ = tree.Scan(context.Background(), "user", []string{"big"})
	if len(rep.Changes) != 1 || rep.Changes[0].Path != "big" {
		t.Errorf("a settled large file: %+v", rep.Changes)
	}
}

// TestNewerStateIsNotSynced checks a root whose state a newer fleet wrote is
// left alone (never read or rewritten): its changes wait, its sides get
// nothing, the status says why, and the other roots sync.
func TestNewerStateIsNotSynced(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "f", "x")
	r.sync()
	r.restartHub()
	for _, id := range r.order {
		r.sides[id].stop()
	}
	r.hub.Close()
	state := filepath.Join(r.base, "hub", "hub", stateFile("user"))
	os.WriteFile(state, []byte(`{"v":99,"seq":1}`+"\n"), 0o600)
	for _, id := range r.order {
		r.sides[id].tree.Close()
	}
	r.openHub()
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
	r.write("host", "user", "g", "waits")
	r.write("host", "tool", "t", "goes")
	r.flushErr(300 * time.Millisecond)
	if got := r.read("c1", "user", "g"); got != "<missing>" {
		t.Errorf("a root of newer state synced: %q", got)
	}
	if got := r.read("c1", "tool", "t"); got != "goes" {
		t.Errorf("the other roots stopped: c1 tool/t = %q", got)
	}
	var broken string
	for _, root := range r.hub.Status().Roots {
		if root.Root == "user" {
			broken = root.Broken
		}
	}
	if !strings.Contains(broken, "newer fleet") {
		t.Errorf("user status Broken = %q", broken)
	}
	if data, _ := os.ReadFile(state); !strings.Contains(string(data), `"v":99`) {
		t.Error("the newer state was rewritten")
	}
}

// TestTreeReopensWithALongLog checks a tree opened on a log long enough to
// be folded right away keeps every record through it (and the next open).
func TestTreeReopensWithALongLog(t *testing.T) {
	dir, state := filepath.Join(t.TempDir(), "user"), t.TempDir()
	os.MkdirAll(dir, 0o755)
	open := func() *Tree {
		tree, err := OpenTree([]TreeRoot{{Name: "user", Dir: dir}}, TreeOptions{StateDir: state, Table: testTable(), Settle: -1})
		if err != nil {
			t.Fatal(err)
		}
		return tree
	}
	tree := open()
	// 800 records, then 400 of them again: a log of 1200 lines, under the
	// fold point of 800 records (1600) but over that of none (1024).
	const files = 800
	for round, n := range []int{files, files / 2} {
		for i := range n {
			os.WriteFile(filepath.Join(dir, fmt.Sprint("f", i)), []byte(fmt.Sprint(round)), 0o644)
		}
		if _, _, err := tree.Scan(context.Background(), "user", nil); err != nil {
			t.Fatal(err)
		}
	}
	if tr := tree.roots["user"]; tr.journal.logged <= 1024 {
		t.Fatalf("log of %d lines: the test needs one past 1024", tr.journal.logged)
	}
	// Crashes: each open's state is left as it is, never folded on close.
	crash := func(tree *Tree) {
		tr := tree.roots["user"]
		tr.journal.log.Close()
		tr.fs.Close()
	}
	crash(tree)
	for range 2 {
		tree = open()
		if n := len(tree.roots["user"].records); n != files {
			t.Fatalf("reopened with %d records, want %d", n, files)
		}
		crash(tree)
	}
}

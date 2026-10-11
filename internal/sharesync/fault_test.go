//go:build unix

package sharesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// faultySide adds a side to a rig behind a faultyPeer.
func (r *rig) faultySide(id, loc string) *faultyPeer {
	r.t.Helper()
	f := &faultyPeer{}
	s := &rigSide{info: SideInfo{ID: id, Location: loc}, dir: filepath.Join(r.base, "sides", id), reserved: map[string][]string{}, peer: f}
	for _, root := range r.roots {
		os.MkdirAll(filepath.Join(s.dir, root), 0o755)
	}
	r.sides[id], r.order = s, append(r.order, id)
	r.openSide(s)
	return f
}

// TestUnreachableSideCatchesUp checks a side that cannot be reached is
// reported behind, and catches up when it is back, nothing lost on the way.
func TestUnreachableSideCatchesUp(t *testing.T) {
	r := newRig(t, "host")
	f := r.faultySide("c1", "k1")
	f.set(func(f *faultyPeer) { f.down = true })
	for i := range 5 {
		r.write("host", "user", "d/"+string(rune('a'+i)), "x")
	}
	err := r.flushErr(300 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "c1") {
		t.Fatalf("Flush with c1 down: %v, want c1 named", err)
	}
	f.set(func(f *faultyPeer) { f.down = false })
	r.hub.Kick("c1")
	r.sync()
	r.assertConverged()
}

// TestLostResultsAreHarmless checks a batch applied whose results never came
// back is sent again and found applied: no conflicts, nothing rewritten.
func TestLostResultsAreHarmless(t *testing.T) {
	r := newRig(t, "host")
	f := r.faultySide("c1", "k1")
	f.set(func(f *faultyPeer) { f.lose = true })
	r.write("host", "user", "f", "one")
	r.flushErr(200 * time.Millisecond)
	before, err := os.Stat(r.path("c1", "user", "f"))
	if err != nil {
		t.Fatalf("the batch was not applied: %v", err)
	}
	f.set(func(f *faultyPeer) { f.lose = false })
	r.hub.Kick("c1")
	r.sync()
	after, _ := os.Stat(r.path("c1", "user", "f"))
	if !os.SameFile(before, after) {
		t.Error("the file was written again")
	}
	if len(r.conflicts()) != 0 {
		t.Errorf("conflicts: %v", r.conflicts())
	}
}

// TestCrashBetweenWriteAndRecord checks a side that stopped after writing an
// op's file and before recording it (a crash) finds the file on restart,
// reports it, and the hub takes it as what it already has.
func TestCrashBetweenWriteAndRecord(t *testing.T) {
	r := newRig(t, "host", "c1")
	var crash atomic.Bool
	crash.Store(true)
	r.sides["c1"].tree.afterWrite = func(root, p string) error {
		if crash.CompareAndSwap(true, false) {
			return errors.New("crash")
		}
		return nil
	}
	r.write("host", "user", "f", "content")
	r.flushErr(200 * time.Millisecond)
	r.restartSide("c1")
	r.sync()
	r.assertConverged()
	if len(r.conflicts()) != 0 {
		t.Errorf("a crash made conflicts: %v", r.conflicts())
	}
	st := r.hub.Status()
	for _, root := range st.Roots {
		if root.Root == "user" && root.Rev != 1 {
			t.Errorf("user rev %d after a crash: the change was taken twice", root.Rev)
		}
	}
}

// droppingReporter loses the hub's answers to one side's reports.
type droppingReporter struct {
	hub  *Hub
	drop atomic.Bool
}

func (d *droppingReporter) Report(ctx context.Context, side string, r Report) (Reply, error) {
	reply, err := d.hub.Report(ctx, side, r)
	if err == nil && d.drop.Load() {
		return Reply{}, errors.New("reply lost")
	}
	return reply, err
}

// TestLostRepliesAreResent checks a report the hub took but whose answer was
// lost is sent again, and taken as the same change: one rev, no conflict.
func TestLostRepliesAreResent(t *testing.T) {
	r := newRig(t, "host", "c1")
	s := r.sides["c1"]
	drop := &droppingReporter{hub: r.hub}
	drop.drop.Store(true)
	s.rep = drop
	r.restartSide("c1")
	r.write("c1", "user", "f", "first")
	if err := s.side.Rescan(context.Background()); err == nil {
		t.Fatal("the lost reply was not noticed")
	}
	r.write("c1", "user", "f", "second, before any answer")
	drop.drop.Store(false)
	r.sync()
	r.assertConverged()
	if got := r.read("host", "user", "f"); got != "second, before any answer" {
		t.Errorf("host user/f = %q", got)
	}
	if len(r.conflicts()) != 0 {
		t.Errorf("a side's own unanswered change conflicted with its next: %v", r.conflicts())
	}
}

// TestTornHubLog checks a hub log cut mid-line (the daemon killed during an
// append) reopens without that line, and the change it was is reported again.
func TestTornHubLog(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "a", "a")
	r.sync()
	for _, id := range r.order {
		r.sides[id].stop()
	}
	// Abandon the hub as a crash would: no compaction.
	for _, id := range r.order {
		r.hub.Detach(id)
	}
	for _, root := range r.hub.order {
		root.mu.Lock()
		root.journal.close()
		root.journal = nil
		root.mu.Unlock()
	}
	for _, id := range r.order {
		r.sides[id].tree.Close()
	}
	log := filepath.Join(r.base, "hub", "hub", stateFile("user")+".log")
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"q":99,"k":"\u0000b","v":{"e":{"k":1`)
	f.Close()
	r.write("host", "user", "b", "b")
	r.openHub()
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
	r.sync()
	r.assertConverged()
	if got := r.read("c1", "user", "b"); got != "b" {
		t.Errorf("c1 user/b = %q", got)
	}
}

// TestLocalWriteRightAfterAnApplyIsReported plays a local writer appending
// to a file in the instant between the side writing an op's content and
// recording it: the record must not take the local line for the op's, so
// the next scan reports it and it reaches everyone.
func TestLocalWriteRightAfterAnApplyIsReported(t *testing.T) {
	for _, grow := range []bool{false, true} {
		t.Run(map[bool]string{false: "rewrite", true: "append"}[grow], func(t *testing.T) {
			r := newRig(t, "host", "c1")
			r.write("host", "tool", "history.jsonl", "one\n")
			r.sync()
			var once atomic.Bool
			p := r.path("c1", "tool", "history.jsonl")
			r.sides["c1"].tree.afterWrite = func(root, name string) error {
				if name == "history.jsonl" && once.CompareAndSwap(false, true) {
					f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						return err
					}
					f.WriteString("local\n")
					f.Close()
				}
				return nil
			}
			if grow {
				r.write("host", "tool", "history.jsonl", "one\ntwo\n")
			} else {
				r.write("host", "tool", "history.jsonl", "rewritten\n")
			}
			r.sync()
			r.sync()
			got := r.read("host", "tool", "history.jsonl")
			if !strings.Contains(got, "local\n") {
				t.Errorf("host history.jsonl = %q: the local line was lost", got)
			}
			r.assertConverged()
		})
	}
}

// crashHub abandons the rig's hub as a killed daemon would: no compaction,
// nothing written but what already was.
func (r *rig) crashHub() {
	for _, id := range r.order {
		r.sides[id].stop()
	}
	for _, id := range r.order {
		r.hub.Detach(id)
	}
	for _, root := range r.hub.order {
		root.mu.Lock()
		if root.journal != nil {
			root.journal.log.Close()
			root.journal = nil
		}
		root.mu.Unlock()
	}
	for _, id := range r.order {
		r.sides[id].tree.Close()
	}
}

// TestAcceptedChangesAreOnDiskBeforeTheAck checks the hub writes every
// change it accepts to its log before it answers: a killed daemon keeps
// every rev it handed out.
func TestAcceptedChangesAreOnDiskBeforeTheAck(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("c1", "user", "f", "acked")
	r.sync()
	r.crashHub()
	h, err := OpenHub(filepath.Join(r.base, "hub"), r.table, r.roots, r.hopts)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	root := h.roots["user"]
	e := root.entries[key{path: "f"}]
	if e == nil || e.Entry.Hash != sum("acked") || e.Rev != 1 || root.rev != 1 {
		t.Fatalf("after a crash the hub holds %+v at rev %d, want f at rev 1", e, root.rev)
	}
	r.hub = h
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
	r.sync()
	r.assertConverged()
}

// TestAttachDistrustsARevGivenOutAgain plays a hub that lost the end of its
// log and handed a rev out again: a side holding that rev for other content
// is not taken to be in sync; the copies are decided, the loser kept.
func TestAttachDistrustsARevGivenOutAgain(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "f", "the hub's rev 1")
	r.sync()
	s := r.sides["c1"]
	s.stop()
	r.hub.Detach("c1")
	s.tree.Close()
	// c1 claims rev 1 for content the hub never had at rev 1.
	r.write("c1", "user", "f", "c1's own rev 1")
	later := time.Now().Add(time.Minute)
	os.Chtimes(r.path("c1", "user", "f"), later, later)
	tree, err := OpenTree([]TreeRoot{{Name: "user", Dir: filepath.Join(s.dir, "user")}}, TreeOptions{StateDir: filepath.Join(r.base, "state", "c1"), Table: r.table})
	if err != nil {
		t.Fatal(err)
	}
	tr := tree.roots["user"]
	info, _ := os.Lstat(r.path("c1", "user", "f"))
	tr.mu.Lock()
	tr.putRecord("f", &record{Rev: 1, Entry: Entry{Kind: File, Mode: 0o644, Size: int64(len("c1's own rev 1")), MTime: later.UnixNano(), Hash: sum("c1's own rev 1")}, FP: fingerprintOf(info)})
	tr.commit()
	tr.mu.Unlock()
	tree.Close()
	r.openSide(s)
	r.sync()
	r.assertConverged()
	if got := r.read("host", "user", "f"); got != "c1's own rev 1" {
		t.Errorf("host user/f = %q, want the newer copy", got)
	}
	if got := r.conflicts(); len(got) != 1 || got[0] != "user/f=the hub's rev 1" {
		t.Errorf("conflicts = %q, want the other copy kept", got)
	}
}

// TestHubStateRegressedBelowASide checks a side holding a rev past the
// hub's counter (the hub lost its log's end) moves the counter past it.
func TestHubStateRegressedBelowASide(t *testing.T) {
	r := newRig(t, "host", "c1")
	for i := range 3 {
		r.write("host", "user", "f", fmt.Sprint("v", i))
		r.sync()
	}
	r.crashHub()
	// Lose the log: the hub is back at its last snapshot (none).
	os.Remove(filepath.Join(r.base, "hub", "hub", stateFile("user")+".log"))
	os.Remove(filepath.Join(r.base, "hub", "hub", stateFile("user")))
	r.openHub()
	r.hub.roots["user"].epoch = r.sides["host"].tree.roots["user"].epoch
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
	r.sync()
	if rev := r.hub.Status().Roots[1].Rev; rev < 3 {
		t.Errorf("user rev %d after a side held rev 3: a rev may be handed out again", rev)
	}
	r.write("c1", "user", "f", "after")
	r.sync()
	r.assertConverged()
}

// TestALostBatchThenADelete checks a file a side wrote from a batch whose
// answer was lost is deleted there when the path is deleted later: the hub
// does not take an unanswered batch for one that never landed.
func TestALostBatchThenADelete(t *testing.T) {
	r := newRig(t, "host")
	f := r.faultySide("c1", "k1")
	f.set(func(f *faultyPeer) { f.lose = true })
	r.write("host", "user", "f", "x")
	r.flushErr(100 * time.Millisecond)
	if got := r.read("c1", "user", "f"); got != "x" {
		t.Fatalf("the lost batch did not land: %q", got)
	}
	// Cut off before the hub tries again: the delete is decided while the
	// hub has no answer for the file.
	f.set(func(f *faultyPeer) { f.down, f.lose = true, false })
	os.Remove(r.path("host", "user", "f"))
	if err := r.sides["host"].side.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *faultyPeer) { f.down = false })
	r.hub.Kick("c1")
	r.sync()
	r.assertConverged()
}

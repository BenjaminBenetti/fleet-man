//go:build unix

package sharesync

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// TestRootOverItsCapPauses checks a change that would take a root past its
// cap waits on the side that made it (paused, never dropped), and goes once
// the root has room again.
func TestRootOverItsCapPauses(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("c1", "small", "one", strings.Repeat("1", 60))
	r.sync()
	r.write("c1", "small", "two", strings.Repeat("2", 60))
	err := r.flushErr(5 * time.Second)
	var ferr *FlushError
	if !errors.As(err, &ferr) || !strings.Contains(err.Error(), "small is over its 100 B cap") {
		t.Fatalf("Flush over the cap: %v, want it to say the root is over its cap", err)
	}
	if got := r.hub.Status().PauseReasons(); len(got) != 1 || got[0] != "small is over its 100 B cap" {
		t.Errorf("PauseReasons = %q", got)
	}
	if got := r.read("host", "small", "two"); got != "<missing>" {
		t.Errorf("the change over the cap reached the host: %q", got)
	}
	if got := r.read("c1", "small", "two"); got != strings.Repeat("2", 60) {
		t.Errorf("the change over the cap was dropped where it was made: %q", got)
	}
	// Room again: the waiting change goes.
	os.Remove(r.path("c1", "small", "one"))
	r.sync()
	r.assertConverged()
	if got := r.read("host", "small", "two"); got != strings.Repeat("2", 60) {
		t.Errorf("host small/two after room came back = %q", got)
	}
	if got := r.hub.Status().PauseReasons(); len(got) != 0 {
		t.Errorf("still paused: %q", got)
	}
}

// TestCustomMountsHaveNoCap checks the defaults' custom mount and host bind
// roots have no cap, and a file over the bulk size syncs there on the bulk
// lane.
func TestCustomMountsHaveNoCap(t *testing.T) {
	r := &rig{t: t, base: t.TempDir(), table: syncpolicy.Defaults(), roots: []string{".mnt", ".hostbind", "files"}, sides: map[string]*rigSide{}, logs: &logBuffer{}}
	r.hopts = HubOptions{NormalDelay: time.Millisecond, BulkDelay: time.Millisecond, Logger: r.logs.logger(), Policies: allPolicies()}
	r.topts = func(string) TreeOptions { return TreeOptions{Settle: -1} }
	r.openHub()
	t.Cleanup(r.close)
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "c1", Location: "c1"})
	for _, root := range r.hub.Status().Roots {
		want := map[string]int64{".mnt": 0, ".hostbind": 0, "files": 64 << 20}[root.Root]
		if root.Cap != want {
			t.Errorf("root %s cap %d, want %d", root.Root, root.Cap, want)
		}
	}
	big := strings.Repeat("m", 3*BulkSize)
	r.write("c1", ".mnt", "opt/data/big.bin", big)
	r.sync()
	if got := r.read("host", ".mnt", "opt/data/big.bin"); got != big {
		t.Errorf("host .mnt/opt/data/big.bin: %d bytes, want %d", len(got), len(big))
	}
}

// allPolicies implements every policy of the defaults, part B's three as
// mirror, for tests of roots that hold them.
func allPolicies() Policies {
	p := DefaultPolicies()
	for _, policy := range []syncpolicy.Policy{syncpolicy.Credential, syncpolicy.JSON3, syncpolicy.TOML3} {
		p[policy] = Mirror{}
	}
	return p
}

// fakeSpace is a disk whose free room a test sets.
type fakeSpace struct {
	free atomic.Int64
}

func (f *fakeSpace) space(string) (Space, error) {
	return Space{Total: 1000 << 20, Free: f.free.Load()}, nil
}

// TestShareFullPausesNotDrops checks a side whose disk cannot take a file is
// paused with its reason (the `sync paused: share full` badge), keeps taking
// deletes, and catches up once it has room.
func TestShareFullPausesNotDrops(t *testing.T) {
	disk := &fakeSpace{}
	disk.free.Store(1000 << 20)
	r := newRig(t)
	r.topts = func(id string) TreeOptions {
		if id == "c1" {
			return TreeOptions{Settle: -1, Space: disk.space, FullReason: "share full"}
		}
		return TreeOptions{Settle: -1}
	}
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "c1", Location: "k1"})
	r.write("host", "user", "old", "old")
	r.sync()

	disk.free.Store(10)
	r.write("host", "user", "new", "a file the share has no room for")
	os.Remove(r.path("host", "user", "old"))
	err := r.flushErr(5 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "paused: share full") {
		t.Fatalf("Flush with the share full: %v", err)
	}
	if got := r.hub.Status().PauseReasons(); len(got) != 1 || got[0] != "share full" {
		t.Errorf("PauseReasons = %q, want share full", got)
	}
	if got := r.read("c1", "user", "old"); got != "<missing>" {
		t.Errorf("a delete did not reach the full share: %q", got)
	}
	if got := r.read("host", "user", "new"); got != "a file the share has no room for" {
		t.Errorf("the host lost the file the share could not take: %q", got)
	}

	disk.free.Store(1000 << 20)
	r.hub.Kick("c1")
	r.sync()
	r.assertConverged()
	if got := r.hub.Status().PauseReasons(); len(got) != 0 {
		t.Errorf("still paused after room came back: %q", got)
	}
}

// TestHostDiskUnderFivePercentPauses checks the daemon host's tree keeps 5%
// of its disk free: a file that would leave less waits, with the reason.
func TestHostDiskUnderFivePercentPauses(t *testing.T) {
	disk := &fakeSpace{}
	disk.free.Store(40 << 20) // 4% of 1000MiB
	const reason = "the daemon host's disk is under 5% free"
	r := newRig(t)
	r.topts = func(id string) TreeOptions {
		if id == "host" {
			return TreeOptions{Settle: -1, Space: disk.space, MinFree: 0.05, FullReason: reason}
		}
		return TreeOptions{Settle: -1}
	}
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "c1", Location: "k1"})
	r.write("c1", "tool", "f", "made in a pod")
	if err := r.flushErr(3 * time.Second); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Flush with the host disk full: %v", err)
	}
	disk.free.Store(500 << 20)
	r.hub.Kick("host")
	r.sync()
	if got := r.read("host", "tool", "f"); got != "made in a pod" {
		t.Errorf("host tool/f = %q", got)
	}
}

// TestNoSpaceErrorsPause checks a write that fails for want of room (ENOSPC,
// EDQUOT) pauses rather than fails.
func TestNoSpaceErrorsPause(t *testing.T) {
	for _, err := range []error{syscall.ENOSPC, syscall.EDQUOT, errNoSpace} {
		if status, _ := classify(&os.PathError{Op: "write", Path: "x", Err: err}); status != NoSpace {
			t.Errorf("classify(%v) = %v, want no space", err, status)
		}
	}
	r := newRig(t, "host", "c1")
	r.write("host", "user", "f", "content")
	tree := r.sides["c1"].tree
	res, err := tree.Apply(context.Background(), Batch{
		Root: "user", Epoch: tree.roots["user"].epoch,
		Ops: []Op{{Path: "f", Rev: 1, Entry: Entry{Kind: File, Mode: 0o644, Size: 7, Hash: sum("content")}}},
		Content: func(context.Context, Op, int64) (io.ReadCloser, error) {
			return nil, &os.PathError{Op: "write", Path: "f", Err: syscall.ENOSPC}
		},
	})
	if err != nil || len(res) != 1 || res[0].Status != NoSpace || res[0].Reason != "disk full" {
		t.Errorf("Apply with ENOSPC: %+v, %v", res, err)
	}
}

// TestATornAppendIsCutBack checks an in-place append the disk ran out of room
// for halfway is cut back to the content the side last synced: the torn copy
// would otherwise be the side's newest change and replace the whole file
// everywhere. The side pauses, then takes the append once it has room.
func TestATornAppendIsCutBack(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "log.jsonl", "one\n")
	r.sync()
	orig := writeAppend
	t.Cleanup(func() { writeAppend = orig })
	writeAppend = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
	}
	f, err := os.OpenFile(r.path("host", "user", "log.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("two\n")
	f.Close()
	if err := r.flushErr(time.Second); err == nil || !strings.Contains(err.Error(), "paused: disk full") {
		t.Fatalf("Flush with the append failing: %v, want c1 paused", err)
	}
	if got := r.read("c1", "user", "log.jsonl"); got != "one\n" {
		t.Errorf("c1 log.jsonl after the torn append = %q, want it cut back", got)
	}
	if rep, _, err := r.sides["c1"].tree.Scan(context.Background(), "user", nil); err != nil || len(rep.Changes) != 0 {
		t.Errorf("c1 reports %+v, %v after the torn append; want nothing", rep.Changes, err)
	}

	writeAppend = orig
	r.hub.Kick("c1")
	r.sync()
	r.assertConverged()
	if got := r.read("host", "user", "log.jsonl"); got != "one\ntwo\n" {
		t.Errorf("host log.jsonl = %q", got)
	}
	if got := r.conflicts(); len(got) != 0 {
		t.Errorf("conflicts: %q", got)
	}
}

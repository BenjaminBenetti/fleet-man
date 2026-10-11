//go:build unix

package sharesync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestChangesReachEverySide covers the plain flow: a file, a folder, a
// symlink, a mode change, an edit and deletes made on one side reach the
// others, and a location-local path stays where it was made.
func TestChangesReachEverySide(t *testing.T) {
	r := newRig(t, "host", "c1", "c2")
	r.write("host", "tool", "a.txt", "one")
	r.write("host", "user", "deep/er/b.txt", "two")
	if err := os.Symlink("deep/er/b.txt", r.path("host", "user", "link")); err != nil {
		t.Fatal(err)
	}
	r.write("host", "tool", "downloads/claude-2.1", "binary")
	r.write("host", "tool", "ide/1.lock", "lock")
	r.sync()
	r.assertConverged()
	if got := r.read("c2", "user", "deep/er/b.txt"); got != "two" {
		t.Errorf("c2 user/deep/er/b.txt = %q", got)
	}
	if got := r.read("c1", "tool", "downloads/claude-2.1"); got != "<missing>" {
		t.Errorf("a location-local download reached c1: %q", got)
	}
	if got := r.read("c1", "tool", "ide/1.lock"); got != "<missing>" {
		t.Errorf("a location-local lock reached c1: %q", got)
	}

	r.write("c1", "tool", "a.txt", "one, edited on c1")
	if err := os.Chmod(r.path("c2", "user", "deep/er/b.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.sync()
	r.assertConverged()
	if got := r.read("host", "tool", "a.txt"); got != "one, edited on c1" {
		t.Errorf("host tool/a.txt = %q", got)
	}
	if info, _ := os.Stat(r.path("host", "user", "deep/er/b.txt")); info.Mode().Perm() != 0o600 {
		t.Errorf("host user/deep/er/b.txt mode %v, want 0600", info.Mode())
	}

	if err := os.RemoveAll(r.path("c2", "user", "deep")); err != nil {
		t.Fatal(err)
	}
	os.Remove(r.path("c2", "tool", "a.txt"))
	r.sync()
	r.assertConverged()
	for _, id := range []string{"host", "c1"} {
		if _, err := os.Lstat(r.path(id, "user", "deep")); !os.IsNotExist(err) {
			t.Errorf("%s user/deep survived the delete: %v", id, err)
		}
	}
	if got := r.read("host", "tool", "downloads/claude-2.1"); got != "binary" {
		t.Errorf("host's own download = %q", got)
	}
	if len(r.conflicts()) != 0 {
		t.Errorf("conflicts without a concurrent change: %v", r.conflicts())
	}
}

// TestConcurrentEditsKeepTheLoser checks mirror's rule: of two edits made
// without seeing each other, the newer wins everywhere and the other is a
// conflict copy; nothing is lost.
func TestConcurrentEditsKeepTheLoser(t *testing.T) {
	// Flush reports c1 before the host: the newer edit arrives first (the
	// older loses as it arrives) or second (it replaces the one there).
	for _, newer := range []string{"c1", "host"} {
		t.Run(newer+" newer", func(t *testing.T) {
			r := newRig(t, "host", "c1")
			r.write("host", "user", "f", "base")
			r.sync()
			older := map[string]string{"c1": "host", "host": "c1"}[newer]
			r.write(older, "user", "f", older+"'s edit")
			r.write(newer, "user", "f", newer+"'s edit, made later")
			later := time.Now().Add(time.Minute)
			os.Chtimes(r.path(newer, "user", "f"), later, later)
			r.sync()
			r.assertConverged()
			if got := r.read("host", "user", "f"); got != newer+"'s edit, made later" {
				t.Errorf("host user/f = %q, want the newer edit", got)
			}
			if got := r.conflicts(); !slices.Equal(got, []string{"user/f=" + older + "'s edit"}) {
				t.Errorf("conflicts = %q, want the older edit", got)
			}
		})
	}
}

// TestAnOpOlderThanTheSideIsRefused checks a side never applies an op older
// than the rev it holds the path at (one sent before the side's own newer
// change was accepted): it would undo that change.
func TestAnOpOlderThanTheSideIsRefused(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("c1", "user", "f", "one")
	r.sync()
	r.write("c1", "user", "f", "two")
	r.sync()
	tree := r.sides["c1"].tree
	tr := tree.roots["user"]
	tr.mu.Lock()
	rev := tr.records["f"].Rev
	tr.mu.Unlock()
	res, err := tree.Apply(context.Background(), Batch{Root: "user", Epoch: tr.epoch, Ops: []Op{{
		Path: "f", Rev: rev - 1, Entry: Entry{Kind: Gone},
	}}})
	if err != nil || len(res) != 1 || res[0].Status != Stale {
		t.Fatalf("an op older than the side: %+v, %v; want stale", res, err)
	}
	if got := r.read("c1", "user", "f"); got != "two" {
		t.Errorf("c1 user/f = %q after an old op", got)
	}
}

// TestStaleDeleteLosesToAnEdit checks a delete made without seeing an edit
// never takes the edit: the file comes back where it was deleted.
func TestStaleDeleteLosesToAnEdit(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "f", "base")
	r.sync()

	// c1 is cut off while the host edits; c1 deletes the old version.
	r.sides["c1"].stop()
	r.hub.Detach("c1")
	r.write("host", "user", "f", "edited")
	r.sync()
	os.Remove(r.path("c1", "user", "f"))
	s := r.sides["c1"]
	s.tree.Close()
	r.openSide(s)
	r.sync()
	r.assertConverged()
	if got := r.read("c1", "user", "f"); got != "edited" {
		t.Errorf("c1 user/f = %q, want the edit back", got)
	}
}

// TestDeleteOfAFolderSomeoneAddedTo checks a folder deleted on one side
// while another added a file in it comes back with the file.
func TestDeleteOfAFolderSomeoneAddedTo(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "d/old", "old")
	r.sync()
	r.sides["c1"].stop()
	r.hub.Detach("c1")
	r.write("host", "user", "d/new", "new")
	r.sync()
	os.RemoveAll(r.path("c1", "user", "d"))
	s := r.sides["c1"]
	s.tree.Close()
	r.openSide(s)
	r.sync()
	r.assertConverged()
	if got := r.read("c1", "user", "d/new"); got != "new" {
		t.Errorf("c1 user/d/new = %q", got)
	}
	if got := r.read("host", "user", "d/old"); got != "<missing>" {
		t.Errorf("host user/d/old = %q, want its delete taken", got)
	}
}

// TestAppendUnionMergesLines checks history logs appended on two sides at
// once end up holding every line everywhere.
func TestAppendUnionMergesLines(t *testing.T) {
	r := newRig(t, "host", "c1", "c2")
	r.write("host", "tool", "history.jsonl", "{\"a\":1}\n")
	r.sync()
	appendTo := func(id, line string) {
		f, err := os.OpenFile(r.path(id, "tool", "history.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(line + "\n")
		f.Close()
	}
	appendTo("host", `{"host":1}`)
	appendTo("c1", `{"c1":1}`)
	appendTo("c2", `{"c2":1}`)
	appendTo("c2", `{"c2":2}`)
	r.sync()
	r.assertConverged()
	got := r.read("host", "tool", "history.jsonl")
	for _, line := range []string{`{"a":1}`, `{"host":1}`, `{"c1":1}`, `{"c2":1}`, `{"c2":2}`} {
		if strings.Count(got, line+"\n") != 1 {
			t.Errorf("history.jsonl holds %q %d times:\n%s", line, strings.Count(got, line+"\n"), got)
		}
	}
	if len(r.conflicts()) != 0 {
		t.Errorf("an append-union merge kept conflict copies: %v", r.conflicts())
	}
}

// TestAppendedFileSendsOnlyItsNewBytes checks a file that grew is appended
// in place on the other sides (the same inode keeps any reader or appender
// that has it open), with only the new bytes fetched.
func TestAppendedFileSendsOnlyItsNewBytes(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("c1", "tool", "projects/t.jsonl", strings.Repeat("x", 4096)+"\n")
	r.sync()
	before, err := os.Stat(r.path("host", "tool", "projects/t.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(r.path("c1", "tool", "projects/t.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("appended\n")
	f.Close()
	counter := &countingPeer{inner: r.sides["c1"].side}
	r.hub.Detach("c1")
	r.sides["c1"].stop()
	r.sides["c1"].peer = counter
	s := r.sides["c1"]
	s.tree.Close()
	r.openSide(s)
	r.sync()
	r.assertConverged()
	after, err := os.Stat(r.path("host", "tool", "projects/t.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the grown file was replaced, not appended to")
	}
	if counter.read != int64(len("appended\n")) {
		t.Errorf("fetched %d bytes for a 9-byte append", counter.read)
	}
}

// TestSidesRestartWithoutResending checks a restarted side and a restarted
// hub pick up where they were: nothing is sent again, nothing conflicts.
func TestSidesRestartWithoutResending(t *testing.T) {
	r := newRig(t, "host", "c1")
	for i := range 20 {
		r.write("host", "user", filepath.Join("d", string(rune('a'+i))), strings.Repeat("y", i))
	}
	r.sync()
	r.restartHub()
	r.restartSide("c1")
	r.sync()
	st := r.hub.Status()
	for _, root := range st.Roots {
		for _, side := range root.Sides {
			if side.Pending != 0 {
				t.Errorf("%s %s: %d pending after a restart", root.Root, side.Side, side.Pending)
			}
		}
	}
	if len(r.conflicts()) != 0 {
		t.Errorf("a restart made conflicts: %v", r.conflicts())
	}
	r.write("c1", "user", "d/a", "after the restarts")
	r.sync()
	r.assertConverged()
	if got := r.read("host", "user", "d/a"); got != "after the restarts" {
		t.Errorf("host user/d/a = %q", got)
	}
}

// TestLostHubStateNeverOverwrites checks a hub whose state is gone (a new
// epoch) takes what every side holds as unseen changes: differing copies
// are decided with conflict copies, never silently overwritten.
func TestLostHubStateNeverOverwrites(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "same", "same")
	r.write("host", "user", "differs", "base")
	r.sync()
	for _, id := range r.order {
		r.sides[id].stop()
	}
	r.hub.Close()
	for _, id := range r.order {
		r.sides[id].tree.Close()
	}
	r.write("c1", "user", "differs", "c1's offline edit")
	later := time.Now().Add(time.Minute)
	os.Chtimes(r.path("c1", "user", "differs"), later, later)
	if err := os.RemoveAll(filepath.Join(r.base, "hub")); err != nil {
		t.Fatal(err)
	}
	r.openHub()
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
	r.sync()
	r.assertConverged()
	if got := r.read("host", "user", "differs"); got != "c1's offline edit" {
		t.Errorf("host user/differs = %q", got)
	}
	if got := r.conflicts(); !slices.Equal(got, []string{"user/differs=base"}) {
		t.Errorf("conflicts = %q, want the host's older copy kept", got)
	}
}

// TestFolderReplacedByAFile checks a folder replaced by a file on one side
// (what was in it now cannot exist there) becomes that file everywhere.
func TestFolderReplacedByAFile(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "d/e/f", "inside")
	r.sync()
	os.RemoveAll(r.path("c1", "user", "d"))
	r.write("c1", "user", "d", "a file now")
	r.sync()
	r.assertConverged()
	if got := r.read("host", "user", "d"); got != "a file now" {
		t.Errorf("host user/d = %q", got)
	}
}

// TestDeletedFolderTakesLocalFiles checks a folder deleted on one side goes
// on another together with the files that side kept for itself there (a
// lock file), as it would from a folder docker instances share.
func TestDeletedFolderTakesLocalFiles(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "tool", "ide/conn.json", "{}")
	r.sync()
	r.write("c1", "tool", "ide/41231.lock", "pid")
	os.RemoveAll(r.path("host", "tool", "ide"))
	r.sync()
	r.assertConverged()
	if _, err := os.Lstat(r.path("c1", "tool", "ide")); !os.IsNotExist(err) {
		t.Errorf("c1 tool/ide survived its delete: %v", err)
	}
}

// TestAnOpNeverUndoesALocalDelete checks an op for a path below a folder the
// side deleted (and has not reported yet) waits rather than make the folder
// again.
func TestAnOpNeverUndoesALocalDelete(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "a/b/f", "x")
	r.sync()
	os.RemoveAll(r.path("c1", "user", "a"))
	tree := r.sides["c1"].tree
	res, err := tree.Apply(context.Background(), Batch{Root: "user", Epoch: tree.roots["user"].epoch, Ops: []Op{{
		Path: "a/b/new", Rev: 99, Entry: Entry{Kind: Dir, Mode: 0o755},
	}}})
	if err != nil || len(res) != 1 || res[0].Status != Changed {
		t.Fatalf("an op below a folder deleted here: %+v, %v; want changed", res, err)
	}
	if _, err := os.Lstat(r.path("c1", "user", "a")); !os.IsNotExist(err) {
		t.Errorf("the deleted folder was made again: %v", err)
	}
}

// TestScanOfAPathBelowAReplacedFolder checks a scan asked for a path whose
// folder was replaced by a file (as an op's Changed result asks) finds the
// replacement, so the side reports it and the hub's op can go.
func TestScanOfAPathBelowAReplacedFolder(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "a/b/f", "x")
	r.sync()
	os.RemoveAll(r.path("c1", "user", "a"))
	r.write("c1", "user", "a", "a file now")
	rep, _, err := r.sides["c1"].tree.Scan(context.Background(), "user", []string{"a/b/f"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Kind{}
	for _, c := range rep.Changes {
		got[c.Path] = c.Entry.Kind
	}
	if got["a"] != File || got["a/b"] != Gone || got["a/b/f"] != Gone {
		t.Errorf("changes = %v, want a a file and a/b, a/b/f gone", got)
	}
}

// TestFolderWithUnseenContentIsNotReplaced checks a folder replaced by a
// file on one side, while another side added to the folder, stays a folder
// with what was added, and the file is kept as a conflict copy.
func TestFolderWithUnseenContentIsNotReplaced(t *testing.T) {
	r := newRig(t, "host", "c1")
	r.write("host", "user", "d/old", "old")
	r.sync()
	s := r.sides["c1"]
	s.stop()
	r.hub.Detach("c1")
	r.write("host", "user", "d/new", "added")
	r.sync()
	os.RemoveAll(r.path("c1", "user", "d"))
	r.write("c1", "user", "d", "a file instead")
	s.tree.Close()
	r.openSide(s)
	r.sync()
	r.assertConverged()
	if got := r.read("c1", "user", "d/new"); got != "added" {
		t.Errorf("c1 user/d/new = %q, want the folder back with what was added", got)
	}
	if got := r.conflicts(); !slices.Equal(got, []string{"user/d=a file instead"}) {
		t.Errorf("conflicts = %q, want the file kept", got)
	}
}

//go:build unix

package sharesync

import (
	"context"
	"os"
	"testing"
)

// TestOnlyKeepsOneFileOfAFolder: a root with Only holds just those names of
// its folder (a bound file's root is the folder it is in): the rest of the
// folder never leaves that side, nor is anything else written into it there.
func TestOnlyKeepsOneFileOfAFolder(t *testing.T) {
	r := newRig(t)
	only := func(s *rigSide) { s.only["user"] = []string{"gitconfig"} }
	r.addSideWith(SideInfo{ID: "host", Location: "host"}, func(s *rigSide) {
		only(s)
		r.sides[s.info.ID] = s
		r.write("host", "user", "gitconfig", "[user]\n")
		r.write("host", "user", ".bashrc", "secret\n")
		r.write("host", "user", "sub/inner", "x\n")
	})
	r.addSideWith(SideInfo{ID: "pod", Location: "k8s"}, only)
	r.sync()
	if got := r.read("pod", "user", "gitconfig"); got != "[user]\n" {
		t.Fatalf("pod gitconfig = %q", got)
	}
	for _, p := range []string{".bashrc", "sub/inner"} {
		if got := r.read("pod", "user", p); got != "<missing>" {
			t.Errorf("pod got %s (%q), which the host's root does not hold", p, got)
		}
	}

	// A file the pod adds beside it never reaches the host's folder.
	r.write("pod", "user", "planted", "evil\n")
	r.write("pod", "user", "gitconfig", "[user]\n\tname = pod\n")
	r.sync()
	if got := r.read("host", "user", "gitconfig"); got != "[user]\n\tname = pod\n" {
		t.Errorf("host gitconfig = %q, want the pod's edit", got)
	}
	if got := r.read("host", "user", "planted"); got != "<missing>" {
		t.Errorf("a file the pod planted beside the bound file reached the host: %q", got)
	}
	if got := r.read("host", "user", ".bashrc"); got != "secret\n" {
		t.Errorf("the host's other file changed: %q", got)
	}
	// Nor does a pod side that holds more than the name (an older spoke,
	// without Only) get it written to the host.
	r.addSide(SideInfo{ID: "old", Location: "k8s2"})
	r.write("old", "user", "planted2", "evil\n")
	r.sync()
	if got := r.read("host", "user", "planted2"); got != "<missing>" {
		t.Errorf("a file a side without Only made reached the host: %q", got)
	}
}

// TestOneWayRootTakesOnlyItsWritersChanges: a one-way root (a read-only
// host bind) takes the host's changes to every side; a pod's change there,
// a new file, an edit or a delete, never reaches the host, and the pod gets
// the host's state back.
func TestOneWayRootTakesOnlyItsWritersChanges(t *testing.T) {
	r := newRig(t)
	r.hub.Close()
	r.hopts.Writers = map[string]string{"user": "host"}
	r.openHub()
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "pod", Location: "k8s"})
	r.addSide(SideInfo{ID: "pod2", Location: "k8s2"})

	r.write("host", "user", "config", "v1\n")
	r.write("pod", "tool", "two-way", "pod\n") // another root: both ways
	r.sync()
	if got := r.read("pod", "user", "config"); got != "v1\n" {
		t.Fatalf("pod config = %q, want the host's", got)
	}
	if got := r.read("host", "tool", "two-way"); got != "pod\n" {
		t.Fatalf("a two-way root did not take the pod's change: %q", got)
	}

	r.write("pod", "user", "config", "pod edit\n")
	r.write("pod", "user", "planted", "x\n")
	r.sync()
	r.sync()
	if got := r.read("host", "user", "config"); got != "v1\n" {
		t.Errorf("host config = %q: a pod's edit of a one-way root reached the host", got)
	}
	if got := r.read("host", "user", "planted"); got != "<missing>" {
		t.Errorf("a pod's new file in a one-way root reached the host: %q", got)
	}
	for _, id := range []string{"pod", "pod2"} {
		if got := r.read(id, "user", "config"); got != "v1\n" {
			t.Errorf("%s config = %q, want the host's state back", id, got)
		}
		if got := r.read(id, "user", "planted"); got != "<missing>" {
			t.Errorf("%s still holds the refused file: %q", id, got)
		}
	}

	if err := os.Remove(r.path("pod", "user", "config")); err != nil {
		t.Fatal(err)
	}
	r.sync()
	r.sync()
	if got := r.read("host", "user", "config"); got != "v1\n" {
		t.Errorf("a pod's delete in a one-way root reached the host: %q", got)
	}
	if got := r.read("pod", "user", "config"); got != "v1\n" {
		t.Errorf("pod config = %q, want the host's file back", got)
	}

	r.write("host", "user", "config", "v2\n")
	r.sync()
	for _, id := range []string{"pod", "pod2"} {
		if got := r.read(id, "user", "config"); got != "v2\n" {
			t.Errorf("%s config = %q, want the host's edit", id, got)
		}
	}
	r.assertConverged()
}

// TestReadOnlyRootIsNeverWritten: a read-only root (a read-only bind's host
// folder) opens without the self-test's writes, reports its changes, and
// writes no op: one of what it holds is recorded, any other answered with
// the folder's own state, which its next scan reports on the op's rev.
func TestReadOnlyRootIsNeverWritten(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/config", []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if os.Geteuid() != 0 {
		if _, err := OpenTree([]TreeRoot{{Name: "user", Dir: dir}}, TreeOptions{StateDir: t.TempDir(), Table: testTable()}); err == nil {
			t.Fatal("a writable root opened on a folder the self-test cannot write")
		}
	}
	tree, err := OpenTree([]TreeRoot{{Name: "user", Dir: dir, ReadOnly: true}}, TreeOptions{StateDir: t.TempDir(), Table: testTable(), Settle: -1})
	if err != nil {
		t.Fatalf("OpenTree(read-only): %v", err)
	}
	defer tree.Close()
	rep, _, err := tree.Scan(context.Background(), "user", nil)
	if err != nil || len(rep.Changes) != 1 || rep.Changes[0].Path != "config" {
		t.Fatalf("Scan = %+v, %v", rep, err)
	}
	res, err := tree.Apply(context.Background(), Batch{Root: "user", Ops: []Op{{Path: "planted", Rev: 1, Entry: Entry{Kind: Dir, Mode: 0o755}}}})
	if err != nil || len(res) != 1 || res[0].Status != Changed {
		t.Fatalf("Apply = %+v, %v; want the folder's own state to stand (Changed)", res, err)
	}
	if _, err := os.Lstat(dir + "/planted"); !os.IsNotExist(err) {
		t.Error("a read-only root was written")
	}
	if err := tree.Acked("user", Reply{Epoch: rep.Epoch, Acks: []Ack{{Path: "config", Entry: rep.Changes[0].Entry, Status: Accepted, Rev: 2}}}); err != nil {
		t.Fatal(err)
	}
	rep, _, err = tree.Scan(context.Background(), "user", []string{"planted"})
	if err != nil || len(rep.Changes) != 1 || rep.Changes[0].Path != "planted" || rep.Changes[0].Entry.Live() || rep.Changes[0].Base != 1 {
		t.Fatalf("the scan after it = %+v, %v; want planted's delete on rev 1", rep, err)
	}
	// An op of what the folder holds is taken as held.
	res, err = tree.Apply(context.Background(), Batch{Root: "user", Ops: []Op{{Path: "config", Rev: 3, Entry: rep0Entry(t, tree)}}})
	if err != nil || len(res) != 1 || res[0].Status != Applied {
		t.Fatalf("Apply of the folder's own content = %+v, %v; want it applied", res, err)
	}
	if got, _ := os.ReadFile(dir + "/config"); string(got) != "v1\n" {
		t.Errorf("config = %q", got)
	}
}

// rep0Entry is the entry tree records for user/config.
func rep0Entry(t *testing.T, tree *Tree) Entry {
	t.Helper()
	tr := tree.roots["user"]
	tr.mu.Lock()
	defer tr.mu.Unlock()
	rec := tr.records["config"]
	if rec == nil {
		t.Fatal("config is not recorded")
	}
	return rec.Entry
}

// TestRootTurnedOneWayTakesItsWritersState: a root that turns one-way (a
// host bind every instance now mounts read-only, after a read-write one)
// converges on the writer's state: a pod's last edit, which the daemon
// host's now read-only folder never took, is undone in every pod, and so is
// a file only a pod made, rather than sent to the host forever.
func TestRootTurnedOneWayTakesItsWritersState(t *testing.T) {
	r := newRig(t)
	r.addSide(SideInfo{ID: "host", Location: "host"})
	r.addSide(SideInfo{ID: "pod", Location: "k8s"})
	r.write("host", "user", "config", "v1\n")
	r.write("host", "user", "kept", "k\n")
	r.sync()
	if got := r.read("pod", "user", "config"); got != "v1\n" {
		t.Fatalf("pod config = %q", got)
	}

	// The host side is away (fleetd restarting) as the pod's last edits of
	// the read-write bind reach the hub.
	host := r.sides["host"]
	host.stop()
	r.hub.Detach("host")
	r.write("pod", "user", "config", "pod edit\n")
	r.write("pod", "user", "planted", "x\n")
	r.sync()

	// The bind turns read-only: the hub opens the root one-way, the host's
	// folder read-only.
	for _, id := range r.order {
		if id != "host" {
			r.sides[id].stop()
		}
	}
	if err := r.hub.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range r.order {
		if err := r.sides[id].tree.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r.hopts.Writers = map[string]string{"user": "host"}
	host.readOnly = map[string]bool{"user": true}
	r.openHub()
	for _, id := range r.order {
		r.openSide(r.sides[id])
	}
	r.sync()
	r.sync()
	if got := r.read("host", "user", "config"); got != "v1\n" {
		t.Fatalf("host config = %q: a read-only folder was written", got)
	}
	if got := r.read("pod", "user", "config"); got != "v1\n" {
		t.Errorf("pod config = %q, want the host's state back", got)
	}
	if got := r.read("pod", "user", "planted"); got != "<missing>" {
		t.Errorf("pod still holds the file the host never had: %q", got)
	}
	if got := r.read("pod", "user", "kept"); got != "k\n" {
		t.Errorf("pod kept = %q", got)
	}
	r.assertConverged()
}

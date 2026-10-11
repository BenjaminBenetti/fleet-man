//go:build unix

package sharesync

import (
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

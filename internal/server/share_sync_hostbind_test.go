//go:build !nok8s && unix

package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/hostbind"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// hostBindShare is a share record with the fleet's own folders and binds of
// a daemon-host folder (read-write) and file (read-only unless rw).
func hostBindShare(claim, aws, git string, gitRW bool) kubernetes.ShareRecord {
	binds := []kubernetes.HostBind{
		{Name: hostbind.Name(aws), Path: aws},
		{Name: hostbind.Name(git), Path: git, File: true, ReadOnly: !gitRW},
	}
	roots := slices.Clone(syncRoots)
	roots = append(roots, podshare.SpokeRoot{Name: hostbind.Name(aws)}, podshare.SpokeRoot{Name: hostbind.Name(git), Only: []string{filepath.Base(git)}})
	return kubernetes.ShareRecord{Mode: "single-node", Claim: claim, Roots: roots, UID: -1, GID: -1, HostBinds: binds}
}

// TestShareSyncKeepsHostBindsAtTheirHostPaths: a bound host folder syncs
// both ways at its real path on the daemon host; a bound read-only file
// follows the host, and a pod's write to its copy never reaches the host
// (the copy gets the host's back), nor does anything else in the file's
// host folder reach the pod; once another instance binds the file
// read-write, its copies write the host too, as on docker.
func TestShareSyncKeepsHostBindsAtTheirHostPaths(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	hostDir := t.TempDir()
	aws, git := filepath.Join(hostDir, "aws"), filepath.Join(hostDir, "gitconfig")
	syncWrite(t, filepath.Join(aws, "config"), "[default]\n")
	syncWrite(t, git, "[user]\n\tname = host\n")
	syncWrite(t, filepath.Join(hostDir, "other"), "never synced\n")
	r.instanceWith(syncCluster, "alpha", "aaaaaaaa", hostBindShare("fs-f-share", aws, git, false), fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	side := shareSide("fs-f-share")
	r.wait()

	awsCopy, gitCopy := r.share(side, hostbind.Name(aws)), r.share(side, hostbind.Name(git))
	if got := syncRead(filepath.Join(awsCopy, "config")); got != "[default]\n" {
		t.Fatalf("pod's aws config = %q", got)
	}
	if got := syncRead(filepath.Join(gitCopy, "gitconfig")); got != "[user]\n\tname = host\n" {
		t.Fatalf("pod's gitconfig = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(gitCopy, "other")); !os.IsNotExist(err) {
		t.Error("a file beside the bound file reached the pod")
	}
	// Nor did the daemon host's side offer the hub anything but the file.
	for _, rs := range r.m.fleetOf(r.fleet).hub.Status().Roots {
		if rs.Root == hostbind.Name(git) && rs.Files != 1 {
			t.Errorf("the bound file's root holds %d files on the hub, want 1", rs.Files)
		}
	}

	// The read-write folder: both ways.
	syncWrite(t, filepath.Join(awsCopy, "sso/cache/token.json"), "token\n")
	r.wait()
	if got := syncRead(filepath.Join(aws, "sso/cache/token.json")); got != "token\n" {
		t.Errorf("a pod's write in the bound folder did not reach the host: %q", got)
	}

	// The read-only file: the pod's write is undone, the host's arrives.
	syncWrite(t, filepath.Join(gitCopy, "gitconfig"), "[user]\n\tname = pod\n")
	syncWrite(t, filepath.Join(gitCopy, "planted"), "x\n")
	r.wait()
	r.wait()
	if got := syncRead(git); got != "[user]\n\tname = host\n" {
		t.Errorf("a pod's write to a read-only bind reached the host: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(hostDir, "planted")); !os.IsNotExist(err) {
		t.Error("a file the pod planted beside a bound file reached the host's folder")
	}
	eventually(t, "the pod's copy of the read-only file is the host's again", func() bool {
		return syncRead(filepath.Join(gitCopy, "gitconfig")) == "[user]\n\tname = host\n"
	})
	syncWrite(t, git, "[user]\n\tname = host 2\n")
	r.wait()
	if got := syncRead(filepath.Join(gitCopy, "gitconfig")); got != "[user]\n\tname = host 2\n" {
		t.Errorf("a host edit of the read-only file did not reach the pod: %q", got)
	}

	// Another instance binds the file read-write: one read-write bind writes
	// the host, whoever else binds it read-only.
	r.instanceWith(syncCluster, "beta", "bbbbbbbb", hostBindShare("fs-f-share", aws, git, true), fleet.StatusRunning)
	r.spokes.up("bbbbbbbb")
	r.m.signal()
	eventually(t, "the file bind turns two-way", func() bool {
		fs := r.m.fleetOf(r.fleet)
		return fs != nil && !fs.binds[hostbind.Name(git)].readOnly
	})
	r.wait()
	syncWrite(t, filepath.Join(gitCopy, "gitconfig"), "[user]\n\tname = pod rw\n")
	r.wait()
	if got := syncRead(git); got != "[user]\n\tname = pod rw\n" {
		t.Errorf("a pod's write to a now read-write bind did not reach the host: %q", got)
	}
	if got := syncRead(filepath.Join(hostDir, "other")); got != "never synced\n" {
		t.Errorf("the bound file's neighbour changed: %q", got)
	}
}

// TestShareSyncLeavesOutAnUnusableHostBind: a host bind whose host folder
// overlaps fleet's own state is never synced; the fleet's other folders
// sync all the same.
func TestShareSyncLeavesOutAnUnusableHostBind(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	syncWrite(t, r.host(".claude/settings.json"), `{"a":1}`)
	own := filepath.Join(os.Getenv("HOME"), ".fleet")
	share := hostBindShare("fs-f-share", own, filepath.Join(t.TempDir(), "gitconfig"), false)
	share.HostBinds = share.HostBinds[:1]
	share.Roots = share.Roots[:len(share.Roots)-1]
	r.instanceWith(syncCluster, "alpha", "aaaaaaaa", share, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	eventually(t, "the fleet's own folders sync", func() bool {
		return syncRead(r.share(shareSide("fs-f-share"), ".claude/settings.json")) == `{"a":1}`
	})
	entries, _ := os.ReadDir(r.share(shareSide("fs-f-share"), hostbind.Name(own)))
	if len(entries) != 0 {
		t.Errorf("fleet's own state reached a pod: %v", entries)
	}
}

// TestHotPathWaitsForBoundFiles: the Ready gate waits for the logins and
// settings, and for bound host files, never for a bound folder's content
// or the fleet's other mirrored folders.
func TestHotPathWaitsForBoundFiles(t *testing.T) {
	git, aws := hostbind.Name("/h/.gitconfig"), hostbind.Name("/h/.aws")
	hot := hotPath(syncpolicy.Defaults(), []podshare.SpokeRoot{{Name: git, Only: []string{".gitconfig"}}, {Name: aws}, {Name: ".claude"}})
	for _, c := range []struct {
		root, path string
		want       bool
	}{
		{git, ".gitconfig", true},
		{aws, "credentials", false},
		{".claude", ".credentials.json", true},
		{".claude", "settings.json", true},
		{".claude", "projects/x.jsonl", false},
		{".mnt", "opt/data/big", false},
	} {
		if got := hot(c.root, c.path); got != c.want {
			t.Errorf("hot(%s, %s) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

// TestShareSyncKeepsEveryInstancesFoldersOnTheShare: a share's spoke keeps
// the folders of every instance that mounts the share, not only of the one
// whose pod runs it: an instance binding a host folder the older instance
// (the spoke's pod) does not gets the host's files, and its writes reach the
// host, also when another cluster's share had that bind already (the
// fleet's folders unchanged).
func TestShareSyncKeepsEveryInstancesFoldersOnTheShare(t *testing.T) {
	r := newSyncRig(t)
	r.hostLayout()
	const otherCluster = "kind-b4c5d6"
	claimA, claimB := "fs-f-1a2b3c4d-share", "fs-f-5e6f7a8b-share"
	sideA, sideB := shareSide(claimA), "k8s:"+otherCluster+"/"+claimB
	aws := filepath.Join(t.TempDir(), "aws")
	syncWrite(t, filepath.Join(aws, "config"), "[default]\n")
	withAWS := func(claim string) kubernetes.ShareRecord {
		s := hostBindShare(claim, aws, filepath.Join(t.TempDir(), "gitconfig"), true)
		s.HostBinds, s.Roots = s.HostBinds[:1], s.Roots[:len(s.Roots)-1]
		return s
	}
	r.instanceWith(syncCluster, "alpha", "aaaaaaaa", withAWS(claimA), fleet.StatusRunning)
	r.instanceOn(otherCluster, "beta", "bbbbbbbb", claimB, syncRoots, fleet.StatusRunning)
	r.spokes.up("aaaaaaaa")
	r.spokes.up("bbbbbbbb")
	r.wait()
	if got := syncRead(filepath.Join(r.share(sideA, hostbind.Name(aws)), "config")); got != "[default]\n" {
		t.Fatalf("alpha's copy of the bound folder = %q", got)
	}

	// gamma binds it on beta's share; beta's pod (the older) runs its spoke.
	r.instanceWith(otherCluster, "gamma", "cccccccc", withAWS(claimB), fleet.StatusRunning)
	r.spokes.up("cccccccc")
	r.m.signal()
	eventually(t, "gamma's copy of the bound folder holds the host's file", func() bool {
		return syncRead(filepath.Join(r.share(sideB, hostbind.Name(aws)), "config")) == "[default]\n"
	})
	syncWrite(t, filepath.Join(r.share(sideB, hostbind.Name(aws)), "from-gamma"), "g\n")
	r.wait()
	if got := syncRead(filepath.Join(aws, "from-gamma")); got != "g\n" {
		t.Errorf("gamma's write in the bound folder did not reach the host: %q", got)
	}
	if served := r.spokes.servedKeys(); slices.Contains(served, "cccccccc") {
		t.Errorf("gamma's pod ran a spoke (%v); beta's, the older, keeps the share", served)
	}
}

// TestHostRootRefusesFleetsOwnStateThroughALink: a bind whose host path
// leads into fleet's own ~/.fleet through a link (or is ~/.fleet's real
// folder when ~/.fleet is a link) never becomes a synced root.
func TestHostRootRefusesFleetsOwnStateThroughALink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(t.TempDir(), "fleet-state")
	if err := os.MkdirAll(filepath.Join(real, "workspaces"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".fleet")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "to-state")
	if err := os.Symlink(filepath.Join(home, ".fleet", "workspaces"), link); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"~/.fleet's real folder": real, "a link into it": link, "its parent": filepath.Dir(real)} {
		if _, _, err := hostRoot(podshare.SpokeRoot{Name: hostbind.Name(p)}, hostBindWant{path: p}); err == nil {
			t.Errorf("%s (%s) became a synced root", name, p)
		}
	}
	ok := filepath.Join(t.TempDir(), "aws")
	if _, _, err := hostRoot(podshare.SpokeRoot{Name: hostbind.Name(ok)}, hostBindWant{path: ok}); err != nil {
		t.Errorf("a plain bound folder: %v", err)
	}
}

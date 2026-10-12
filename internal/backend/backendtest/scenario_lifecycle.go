package backendtest

import (
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// lifecycleTimeout bounds how long Status may take to catch up with a stop,
// start or rebuild.
const lifecycleTimeout = 2 * time.Minute

// Paths the scenarios leave state in, inside an instance.
const (
	homeMarker  = ".fleet-conformance/home-marker"
	siblingRepo = "/workspaces/conformance-sibling"
	packageName = "fleet-conformance"
	packageFile = "/usr/share/fleet-conformance/marker"
)

// installPackage builds a one-file .deb in the instance and installs it with
// dpkg, the way apt leaves a package: in the root filesystem, outside $HOME
// and the workspace. No network needed. dpkg-deb refuses a group- or
// world-writable control folder, and an instance's umask may be 000.
const installPackage = `set -e
umask 022
d=$(mktemp -d)
mkdir -p "$d/pkg/DEBIAN" "$d/pkg/usr/share/fleet-conformance"
printf 'Package: fleet-conformance\nVersion: 1.0\nArchitecture: all\nMaintainer: fleet <fleet@example.invalid>\nDescription: fleet backend conformance marker\n' > "$d/pkg/DEBIAN/control"
printf '%s\n' "$1" > "$d/pkg/usr/share/fleet-conformance/marker"
dpkg-deb --root-owner-group --build "$d/pkg" "$d/fleet-conformance.deb" >/dev/null
sudo -n dpkg -i "$d/fleet-conformance.deb" >/dev/null
rm -rf "$d"`

// makeSiblingRepo commits a repo beside the workspace under /workspaces, where
// a user clones a second repo.
const makeSiblingRepo = `set -e
sudo -n mkdir -p "$2"
sudo -n chown "$(id -u):$(id -g)" "$2"
cd "$2"
git init -q
printf '%s\n' "$1" > sibling.txt
git add sibling.txt
git -c user.name=conformance -c user.email=conformance@example.invalid commit -qm sibling`

// leaveState writes marker into the instance's $HOME and workspace, and on a
// target with root into its root filesystem (a dpkg package) and a sibling repo
// under /workspaces. checkState expects it all back.
func leaveState(c *C, i *Instance, marker string) {
	c.Helper()
	c.Sh(i, `mkdir -p "$(dirname "$HOME/$2")" && printf '%s' "$1" > "$HOME/$2"`, marker, homeMarker)
	c.Sh(i, `printf '%s' "$1" > lifecycle-keep.txt`, marker)
	if c.Target().HostProcesses {
		c.Logf("instance commands run on this host: no root filesystem or /workspaces to leave state in")
		return
	}
	c.Sh(i, installPackage, marker)
	c.Sh(i, makeSiblingRepo, marker, siblingRepo)
}

// checkState expects what leaveState wrote, after the step named after.
func checkState(c *C, i *Instance, marker, after string) {
	c.Helper()
	expect := func(what, got string) {
		c.Helper()
		if got != marker {
			c.Errorf("after %s the %s reads %q, want %q", after, what, got, marker)
		}
	}
	expect("$HOME file", c.Sh(i, `cat "$HOME/$1" 2>/dev/null || true`, homeMarker))
	expect("uncommitted workspace file", c.Sh(i, `cat lifecycle-keep.txt 2>/dev/null || true`))
	if c.Target().HostProcesses {
		return
	}
	if got := c.Sh(i, `dpkg-query -W -f='${Status}' "$1" 2>/dev/null || true`, packageName); got != "install ok installed" {
		c.Errorf("after %s the package installed with dpkg is %q, want installed", after, got)
	}
	expect("installed package's file", c.Sh(i, `cat "$1" 2>/dev/null || true`, packageFile))
	expect("sibling repo's file", c.Sh(i, `cat "$1/sibling.txt" 2>/dev/null || true`, siblingRepo))
	if got := c.Sh(i, `git -C "$1" log -1 --format=%s 2>/dev/null || true`, siblingRepo); got != "sibling" {
		c.Errorf("after %s the sibling repo's last commit is %q, want %q", after, got, "sibling")
	}
	if out := c.Sh(i, `dpkg --audit 2>&1 || true`); out != "" {
		c.Errorf("after %s dpkg --audit reports:\n%s", after, out)
	}
}

// stop stops the instance and waits for Status to say so.
func stop(c *C, i *Instance) {
	c.Helper()
	if err := i.B.Stop(i.ID()); err != nil {
		c.Fatalf("Stop: %v", err)
	}
	i.Record.Status = fleet.StatusStopped
	i.Rebind()
	waitStatus(c, i, i.ID(), backend.LiveStatusStopped, lifecycleTimeout)
}

// caseStopStart: a stopped instance keeps its container and everything in
// it: $HOME, the workspace, the root filesystem (an installed package) and a
// sibling repo under /workspaces. Start runs postStartCommand once more and
// postCreateCommand not at all; commands fail while it is stopped.
func caseStopStart(c *C) {
	i := c.NewInstance(Fixture{})
	marker := randHex(6)
	leaveState(c, i, marker)
	id := i.ID()

	stop(c, i)
	if r := c.Exec(i, []string{"true"}, nil); r.Err == nil {
		c.Errorf("ExecCommand succeeded on a stopped instance")
	}
	if err := i.B.Start(id); err != nil {
		c.Fatalf("Start: %v", err)
	}
	i.Record.Status = fleet.StatusRunning
	i.Rebind()
	waitStatus(c, i, id, backend.LiveStatusRunning, lifecycleTimeout)

	checkState(c, i, marker, "a stop and start")
	if n := lineCount(c, i, PostStartLog); n != 2 {
		c.Errorf("postStartCommand ran %d times over a create and a start, want 2 (once per start)", n)
	}
	if n := lineCount(c, i, PostCreateLog); n != 1 {
		c.Errorf("postCreateCommand ran %d times over a create and a start, want 1", n)
	}
}

// rebuild creates an instance, stops it when fromStopped, and rebuilds it
// through RebuildContext as a rebuild job does: it must come back running in a
// new container, the old one gone and the workspace kept.
func rebuild(c *C, fromStopped bool) {
	c.Helper()
	i := c.NewInstance(Fixture{})
	if !i.B.SupportsRebuild() {
		c.Skipf("%s cannot rebuild", c.Target().Name)
	}
	marker := randHex(6)
	c.Sh(i, `printf '%s' "$1" > rebuild-keep.txt`, marker)
	if fromStopped {
		stop(c, i)
	}
	old := i.ID()
	log := &lockedBuffer{}
	r, err := backend.RebuildContext(backend.WithJobLog(c.Ctx, log), i.B, old, backend.UpRequest{WorkspaceDir: i.WorkspaceDir()})
	if err != nil {
		c.Fatalf("Rebuild: %v%s", err, log.tail(40))
	}
	if r == nil || r.ContainerID == "" {
		c.Fatalf("Rebuild succeeded without a ContainerID")
	}
	i.SetResult(r)
	if i.ID() == old {
		c.Errorf("Rebuild kept the ContainerID %s; a rebuilt instance gets a new one", old)
	} else {
		waitStatus(c, i, old, backend.LiveStatusMissing, lifecycleTimeout)
	}
	waitStatus(c, i, i.ID(), backend.LiveStatusRunning, lifecycleTimeout)
	if got := c.Sh(i, `cat rebuild-keep.txt 2>/dev/null || true`); got != marker {
		c.Errorf("after the rebuild the uncommitted workspace file reads %q, want %q", got, marker)
	}
	if got := c.Sh(i, `cat "$1"`, SeedFile); got != strings.TrimRight(SeedContent, "\n") {
		c.Errorf("after the rebuild the workspace's %s reads %q", SeedFile, got)
	}
	if n := lineCount(c, i, PostStartLog); n < 1 {
		c.Errorf("postStartCommand did not run in the rebuilt instance")
	}
}

// caseRebuild: rebuilding a running instance replaces its container and keeps
// its workspace.
func caseRebuild(c *C) { rebuild(c, false) }

// caseRebuildStopped: so does rebuilding a stopped one, which comes back
// running.
func caseRebuildStopped(c *C) { rebuild(c, true) }

// caseRebuildRollback: on a backend that keeps the previous generation until
// the new one runs, a rebuild that fails (the devcontainer.json edited in the
// instance gets a postCreateCommand that exits non-zero) is undone: the error
// carries the hook's output, the instance runs its previous container again
// with the root filesystem it had, and the workspace keeps the edit (to fix,
// then rebuild again).
func caseRebuildRollback(c *C) {
	c.Helper()
	if !c.Target().RebuildRollsBack {
		c.Skipf("%s removes the old container before it builds the new one: a failed rebuild has nothing to roll back to", c.Target().Name)
	}
	const said = "conformance-rebuild-rollback"
	i := c.NewInstance(Fixture{})
	marker := randHex(6)
	c.Sh(i, `printf '%s' "$1" > "$HOME/rebuild-rollback-root"`, marker)
	config := "/workspaces/" + FixtureRepo + "/.devcontainer/devcontainer.json"
	c.Sh(i, `sed -i 's/^\(  "postCreateCommand": \).*$/\1"echo `+said+` >\&2; exit 7",/' "$1" && grep -q `+said+` "$1"`, config)
	old := i.ID()
	log := &lockedBuffer{}
	_, err := backend.RebuildContext(backend.WithJobLog(c.Ctx, log), i.B, old, backend.UpRequest{WorkspaceDir: i.WorkspaceDir()})
	if err == nil {
		c.Fatalf("the rebuild succeeded though its postCreateCommand fails")
	}
	if !strings.Contains(err.Error(), said) {
		c.Errorf("the rebuild error does not carry the failing command's output %q:\n%v", said, err)
	}
	waitStatus(c, i, old, backend.LiveStatusRunning, lifecycleTimeout)
	if got := c.Sh(i, `cat "$HOME/rebuild-rollback-root" 2>/dev/null || true`); got != marker {
		c.Errorf("after the failed rebuild the previous root's file reads %q, want %q (the generation was not restored)", got, marker)
	}
	if got := c.Sh(i, `grep -c `+said+` "$1" || true`, config); got != "1" {
		c.Errorf("after the failed rebuild the workspace lost the edit (grep -c = %q)", got)
	}
	if got := c.Sh(i, `cat "$1"`, SeedFile); got != strings.TrimRight(SeedContent, "\n") {
		c.Errorf("after the failed rebuild the workspace's %s reads %q", SeedFile, got)
	}
}

// caseFailedCreate: a create whose postCreateCommand fails reports the
// failure with the command's output, and the daemon's teardown of the failed
// record (which has no ContainerID) leaves nothing behind.
func caseFailedCreate(c *C) {
	const said = "conformance-failed-create"
	i, err := c.Provision(Fixture{PostCreate: "echo " + said + " >&2; exit 7"})
	if err == nil {
		c.Fatalf("the create succeeded though postCreateCommand failed")
	}
	if !strings.Contains(err.Error(), said) {
		c.Errorf("the create error does not carry the failing command's output %q:\n%v", said, err)
	}
	if i.ID() != "" {
		c.Errorf("a failed create recorded ContainerID %q", i.ID())
	}
	c.Teardown(i)
}

// blockingCreateBound bounds a create whose login shell never answers: the
// devcontainer CLI gives its userEnvProbe up after 10s, so such a create
// takes about as long as any other.
const blockingCreateBound = 10 * time.Minute

// caseBlockingLoginShell: a create whose remote user's interactive login
// shell never answers (an rc file waiting for input) still ends, and
// succeeds: the devcontainer CLI gives its userEnvProbe (`bash -lic`) up
// after 10s and runs the lifecycle hooks without the probed environment, as
// `devcontainer up` does on docker. The instance then runs commands.
func caseBlockingLoginShell(c *C) {
	type created struct {
		i   *Instance
		err error
	}
	done := make(chan created, 1)
	start := time.Now()
	go func() {
		i, err := c.suite.provision(c.Ctx, Fixture{BlockingLoginShell: true})
		done <- created{i, err}
	}()
	var got created
	select {
	case got = <-done:
	case <-time.After(blockingCreateBound):
		c.Fatalf("the create is still running %s after it began: it hangs on a login shell that never answers (the devcontainer CLI gives its userEnvProbe up after 10s)", blockingCreateBound)
	}
	if got.i != nil {
		c.Cleanup(func() { c.suite.teardown(c, got.i) })
	}
	if got.err != nil {
		c.Fatalf("a create whose login shell never answers failed after %s; the devcontainer CLI runs the hooks without the probed environment instead: %v", time.Since(start).Round(time.Second), got.err)
	}
	c.Logf("the create took %s", time.Since(start).Round(time.Second))
	i := got.i
	if n := lineCount(c, i, PostCreateLog); n != 1 {
		c.Errorf("postCreateCommand ran %d times, want 1: the hooks run without the probed environment", n)
	}
	c.Sh(i, `touch "$1"`, UnblockLoginShell)
}

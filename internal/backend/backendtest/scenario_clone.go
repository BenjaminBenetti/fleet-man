package backendtest

import (
	"os"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// commitSeed makes the workspace a git repo with the seed committed.
const commitSeed = `set -e
git init -q .
git add "$1"
git -c user.name=conformance -c user.email=conformance@example.invalid commit -qm seed`

// caseClone: a clone of a running instance starts with everything the source
// had (its $HOME, its workspace with an uncommitted file and a git history
// that passes fsck, its root filesystem with an installed package and a
// sibling repo), while the source keeps running and the two are independent.
func caseClone(c *C) {
	src := c.NewInstance(Fixture{})
	if !src.B.SupportsClone() {
		c.Skipf("%s cannot clone", c.Target().Name)
	}
	marker := randHex(6)
	hasGit := c.NeedTool(src, "git")
	if hasGit {
		c.Sh(src, commitSeed, SeedFile)
	}
	leaveState(c, src, marker)

	dest := cloneOf(c, src)
	if dest.ID() == src.ID() {
		c.Fatalf("the clone has the source's ContainerID %s", src.ID())
	}
	waitStatus(c, dest, dest.ID(), backend.LiveStatusRunning, lifecycleTimeout)
	checkState(c, dest, marker, "a clone")
	if hasGit {
		if r := c.Exec(dest, ShArgv("git fsck --full --no-progress"), nil); r.Err != nil {
			c.Errorf("git fsck in the clone's workspace: %s", r)
		}
		if got := c.Sh(dest, "git log -1 --format=%s"); got != "seed" {
			c.Errorf("the clone's workspace history ends at %q, want %q", got, "seed")
		}
		if got := c.Sh(dest, "git status --porcelain -- lifecycle-keep.txt"); got != "?? lifecycle-keep.txt" {
			c.Errorf("the clone's uncommitted file shows in git status as %q, want untracked", got)
		}
	}

	if got := src.B.Status(src.ID()); got != backend.LiveStatusRunning {
		c.Errorf("the source is %q after the clone, want %q", got, backend.LiveStatusRunning)
	}
	c.Sh(dest, `printf x > "$HOME/.fleet-conformance/clone-only"`)
	if r := c.Exec(src, ShArgv(`test -e "$HOME/.fleet-conformance/clone-only"`), nil); r.Err == nil {
		c.Errorf("a file written in the clone's $HOME showed up in the source's")
	}
}

// caseCloneStopped: a stopped instance clones too (on a cluster, a CSI clone
// of its disk where the driver can, else a copy), and stays stopped.
func caseCloneStopped(c *C) {
	src := c.NewInstance(Fixture{})
	if !src.B.SupportsClone() {
		c.Skipf("%s cannot clone", c.Target().Name)
	}
	marker := randHex(6)
	leaveState(c, src, marker)
	stop(c, src)

	dest := cloneOf(c, src)
	waitStatus(c, dest, dest.ID(), backend.LiveStatusRunning, lifecycleTimeout)
	checkState(c, dest, marker, "a clone of a stopped instance")
	if got := src.B.Status(src.ID()); got != backend.LiveStatusStopped {
		c.Errorf("the source is %q after the clone, want it left %q", got, backend.LiveStatusStopped)
	}
}

// churnLoop is a writer the consistency case runs in the source while it is
// cloned: commits in a repo in $HOME and appends to a file, as fast as it
// can, detached from the exec that starts it. $HOME is on the instance's root
// filesystem, which a clone takes at one point in time (docker commit pauses
// the container); a devcontainer's workspace is a host directory a clone
// copies live.
const churnLoop = `set -e
cd "$HOME"
git init -q churn-repo
cd churn-repo
git -c user.name=c -c user.email=c@example.invalid commit -q --allow-empty -m 0
setsid sh -c 'n=0; while [ ! -e ../churn.stop ]; do n=$((n+1)); echo "$n" >> ../churn.log; echo "$n" > f; git add f; git -c user.name=c -c user.email=c@example.invalid commit -qm "$n" || true; done' >/dev/null 2>&1 </dev/null &`

// caseCloneConsistent: an instance whose pod can be held still (privileged,
// as docker-in-docker's are) clones consistently while it writes: the git
// repo it commits to in $HOME passes fsck in the clone, and the clone's dpkg
// database --audit, wherever the copy was cut. The source runs on.
func caseCloneConsistent(c *C) {
	src := c.NewInstance(Fixture{Privileged: true})
	if !src.B.SupportsClone() {
		c.Skipf("%s cannot clone", c.Target().Name)
	}
	if c.Target().HostProcesses || !c.NeedTool(src, "git") || !c.NeedTool(src, "setsid") {
		c.Skipf("no git repo or detached writer in %s's instances", c.Target().Name)
	}
	marker := randHex(6)
	leaveState(c, src, marker)
	c.Sh(src, churnLoop)
	c.Eventually("the source's writer to commit", lifecycleTimeout, func() (bool, string) {
		n := c.Sh(src, `git -C "$HOME/churn-repo" rev-list --count HEAD 2>/dev/null || echo 0`)
		return n != "0" && n != "1", n + " commits"
	})

	dest := cloneOf(c, src)
	waitStatus(c, dest, dest.ID(), backend.LiveStatusRunning, lifecycleTimeout)
	c.Sh(src, `touch "$HOME/churn.stop"`)
	checkState(c, dest, marker, "a clone of an instance that wrote throughout")
	if r := c.Exec(dest, ShArgv(`git -C "$HOME/churn-repo" fsck --full --no-progress`), nil); r.Err != nil {
		c.Errorf("git fsck of the repo the source committed to throughout the clone: %s", r)
	}
	if got := c.Sh(dest, `git -C "$HOME/churn-repo" rev-list --count HEAD`); got == "" || got == "0" {
		c.Errorf("the clone's repo has %q commits", got)
	}
	if got := src.B.Status(src.ID()); got != backend.LiveStatusRunning {
		c.Errorf("the source is %q after the clone, want %q", got, backend.LiveStatusRunning)
	}
	if strings.Contains(strings.Join(dest.resultWarnings(), "\n"), "best-effort") {
		c.Errorf("a clone of a pod that can be held still is best-effort: %v", dest.resultWarnings())
	}
}

// cloneOf clones src the way a clone job does: the destination record is bound
// (no ContainerID yet), the source's workspace is copied on the host unless
// the backend owns its workspaces, then Clone. The clone is torn down when the
// case ends.
func cloneOf(c *C, src *Instance) *Instance {
	c.Helper()
	rec := c.suite.newRecord("clone")
	rec.Backend = src.Record.Backend
	dest := &Instance{Record: rec, target: c.Target()}
	dest.Rebind()
	c.Cleanup(func() { c.suite.teardown(c, dest) })

	if !backend.OwnsWorkspace(dest.B) {
		// As create.copyWorkspaceTree does it.
		if err := os.MkdirAll(rec.WorkspaceDir, 0o755); err != nil {
			c.Fatalf("clone workspace: %v", err)
		}
		if out, err := exec.CommandContext(c.Ctx, "cp", "-a", src.WorkspaceDir()+"/.", rec.WorkspaceDir+"/").CombinedOutput(); err != nil {
			c.Fatalf("copy the workspace: %v\n%s", err, out)
		}
	}
	r, err := backend.CloneContext(c.Ctx, dest.B, src.ID(), backend.UpRequest{WorkspaceDir: rec.WorkspaceDir})
	if err != nil {
		c.Fatalf("Clone: %v", err)
	}
	if r == nil || r.ContainerID == "" {
		c.Fatalf("Clone succeeded without a ContainerID")
	}
	dest.SetResult(r)
	c.Logf("cloned %s into %s", src.ID(), r.ContainerID)
	return dest
}

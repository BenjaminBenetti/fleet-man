package backendtest

import (
	"os"
	"os/exec"

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
	r, err := dest.B.Clone(src.ID(), rec.WorkspaceDir, nil)
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

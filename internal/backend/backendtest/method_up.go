package backendtest

import (
	"os"
	"path"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// caseUp: a create gives a running instance whose UpResult names its
// container, user and workspace folder; commands run in that folder, as that
// user, on the fixture's workspace; each create-time lifecycle command ran
// once.
func caseUp(c *C) {
	i := c.Shared()
	r := i.Result
	if r.ContainerID == "" {
		c.Errorf("UpResult.ContainerID is empty")
	}
	if r.RemoteUser == "" {
		c.Errorf("UpResult.RemoteUser is empty")
	}
	if !path.IsAbs(r.RemoteWorkspaceFolder) {
		c.Errorf("UpResult.RemoteWorkspaceFolder = %q, want an absolute path", r.RemoteWorkspaceFolder)
	}
	if got := i.B.Status(i.ID()); got != backend.LiveStatusRunning {
		c.Errorf("Status after Up = %q, want %q", got, backend.LiveStatusRunning)
	}
	if got := c.Sh(i, "pwd"); got != r.RemoteWorkspaceFolder {
		c.Errorf("commands run in %q, want the remote workspace folder %q", got, r.RemoteWorkspaceFolder)
	}
	if got := c.Sh(i, `cat "$1"`, SeedFile); got != strings.TrimRight(SeedContent, "\n") {
		c.Errorf("the workspace's %s reads %q, want the fixture's %q", SeedFile, got, SeedContent)
	}
	if got := c.Sh(i, "id -un"); got != r.RemoteUser {
		c.Errorf("commands run as %q, want UpResult.RemoteUser %q", got, r.RemoteUser)
	}
	for _, log := range []string{PostCreateLog, PostStartLog} {
		if n := lineCount(c, i, log); n != 1 {
			c.Errorf("$HOME/%s has %d lines after the create, want 1 (the lifecycle command runs once)", log, n)
		}
	}
}

// caseUpAgain: Up on a workspace that already has a container (a create run
// again after a crash) adopts that container or prunes it; it never leaves a
// second one behind. The teardown's empty Leftovers proves nothing was
// orphaned.
func caseUpAgain(c *C) {
	i := c.NewInstance(Fixture{})
	first := i.ID()

	again := *i.Record
	again.ContainerID = ""
	b := c.Target().Bind(&again)
	r, err := backend.UpContext(c.Ctx, b, backend.UpRequest{WorkspaceDir: i.WorkspaceDir()})
	if err != nil {
		c.Fatalf("Up again on the same workspace: %v", err)
	}
	i.SetResult(r)
	if r.ContainerID != first {
		c.Logf("Up again pruned %s and made %s", first, r.ContainerID)
		if got := i.B.Status(first); got != backend.LiveStatusMissing {
			c.Errorf("Up again made %s but left the first container %s %q", r.ContainerID, first, got)
		}
	} else {
		c.Logf("Up again adopted %s", first)
	}
	if got := i.B.Status(r.ContainerID); got != backend.LiveStatusRunning {
		c.Errorf("Status after the second Up = %q, want %q", got, backend.LiveStatusRunning)
	}
	if got := c.Sh(i, `cat "$1"`, SeedFile); got != strings.TrimRight(SeedContent, "\n") {
		c.Errorf("after the second Up the workspace's %s reads %q", SeedFile, got)
	}
}

// caseCapabilities: the capability answers agree with each other (the
// capability-only handle and the bound backend, SupportsMicSink and
// MicSinkCommand), and a backend refuses what it says it cannot do.
func caseCapabilities(c *C) {
	i := c.Shared()
	b := i.B
	if ask := c.Target().Capabilities; ask != nil {
		caps := ask(i.Record)
		for _, q := range []struct {
			name      string
			asked, of bool
		}{
			{"SupportsSharedMounts", caps.SupportsSharedMounts(), b.SupportsSharedMounts()},
			{"SupportsClone", caps.SupportsClone(), b.SupportsClone()},
			{"SupportsRebuild", caps.SupportsRebuild(), b.SupportsRebuild()},
			{"SupportsMicSink", caps.SupportsMicSink(), b.SupportsMicSink()},
		} {
			if q.asked != q.of {
				c.Errorf("%s: the capability handle says %v, the bound backend %v", q.name, q.asked, q.of)
			}
		}
	}

	cmd, ok := b.MicSinkCommand(i.ID())
	switch {
	case ok != b.SupportsMicSink():
		c.Errorf("MicSinkCommand ok = %v, SupportsMicSink = %v; they must agree", ok, b.SupportsMicSink())
	case ok && cmd == nil:
		c.Errorf("MicSinkCommand returned ok with no command")
	case !ok && cmd != nil:
		c.Errorf("MicSinkCommand returned a command with ok = false")
	}
	if ob, isOutput := b.(backend.OutputBackend); isOutput {
		if cmd, ok := ob.OutputSourceCommand(i.ID()); ok && cmd == nil {
			c.Errorf("OutputSourceCommand returned ok with no command")
		}
	}

	// A refusal is all that is asked; a scratch workspace keeps a wrong
	// "success" away from the shared instance.
	scratch, err := os.MkdirTemp(c.suite.root, "refuse-")
	if err != nil {
		c.Fatalf("scratch workspace: %v", err)
	}
	if !b.SupportsClone() {
		if r, err := b.Clone(i.ID(), scratch, nil); err == nil {
			c.Errorf("Clone succeeded though SupportsClone is false")
			discard(b, r)
		}
	}
	if !b.SupportsRebuild() {
		if r, err := b.Rebuild("", scratch, nil); err == nil {
			c.Errorf("Rebuild succeeded though SupportsRebuild is false")
			discard(b, r)
		}
	}
}

// discard removes what a wrongly successful call made.
func discard(b backend.Backend, r *backend.UpResult) {
	if r != nil && r.ContainerID != "" {
		_ = b.Down(r.ContainerID)
	}
}

// caseStatus: Status reports the running shared instance as running, and an
// empty ContainerID as unknown (inconclusive, so callers keep what they had).
// Stopped is checked by stop-start, missing by every teardown.
func caseStatus(c *C) {
	i := c.Shared()
	if got := i.B.Status(i.ID()); got != backend.LiveStatusRunning {
		c.Errorf("Status(%s) = %q, want %q", i.ID(), got, backend.LiveStatusRunning)
	}
	if got := i.B.Status(""); got != backend.LiveStatusUnknown {
		c.Errorf("Status(\"\") = %q, want %q", got, backend.LiveStatusUnknown)
	}
}

// lineCount is the number of lines in the $HOME-relative file (0 when it is
// missing).
func lineCount(c *C, i *Instance, rel string) int {
	c.Helper()
	out := strings.TrimSpace(c.Sh(i, `if [ -f "$HOME/$1" ]; then wc -l < "$HOME/$1"; else echo 0; fi`, rel))
	n := 0
	for _, r := range out {
		if r < '0' || r > '9' {
			c.Fatalf("line count of $HOME/%s: %q", rel, out)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// waitStatus waits for Status to report want.
func waitStatus(c *C, i *Instance, id string, want backend.LiveStatus, timeout time.Duration) {
	c.Helper()
	c.Eventually("Status("+id+") = "+string(want), timeout, func() (bool, string) {
		got := i.B.Status(id)
		return got == want, "Status = " + string(got)
	})
}

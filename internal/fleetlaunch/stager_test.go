package fleetlaunch

import (
	"errors"
	"testing"
)

// stagingBackend is a recordingBackend that stages the binary itself.
type stagingBackend struct {
	recordingBackend
	workspaceDir string
	calls        int
	hookRan      bool
	staged       bool
	err          error
}

func (b *stagingBackend) StageFleet(workspaceDir string, beforeRefresh func() error) (bool, error) {
	b.calls++
	b.workspaceDir = workspaceDir
	if beforeRefresh != nil {
		b.hookRan = beforeRefresh() == nil
	}
	return b.staged, b.err
}

// TestEnsureFreshHandsAStagerTheWholeJob: a backend that stages the binary
// its own way gets the call, hook included, and fleetlaunch neither probes the
// instance nor copies the daemon's executable into it.
func TestEnsureFreshHandsAStagerTheWholeJob(t *testing.T) {
	b := &stagingBackend{staged: true}
	staged, err := EnsureFresh(b, "/ws/f/i", func() error { return nil })
	if err != nil || !staged {
		t.Fatalf("EnsureFresh = %v, %v; want the Stager's true, nil", staged, err)
	}
	if b.calls != 1 || b.workspaceDir != "/ws/f/i" || !b.hookRan {
		t.Fatalf("StageFleet calls %d, workspace %q, hook ran %v; want one call for /ws/f/i with the hook", b.calls, b.workspaceDir, b.hookRan)
	}
	if len(b.execScripts) != 0 || b.copiedTo != "" {
		t.Fatalf("fleetlaunch staged as well: execs %v, copied to %q", b.execScripts, b.copiedTo)
	}

	b = &stagingBackend{err: errors.New("no fleet-pod binary for arm64")}
	if staged, err := EnsureFresh(b, "/ws/f/i", nil); err == nil || staged {
		t.Fatalf("EnsureFresh = %v, %v; want the Stager's error", staged, err)
	}
}

// TestEnsureFreshStagesItselfWithoutAStager: every other backend still gets
// the probe and the copy.
func TestEnsureFreshStagesItselfWithoutAStager(t *testing.T) {
	b := &recordingBackend{}
	if _, err := EnsureFresh(b, "/ws", nil); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if len(b.execScripts) == 0 {
		t.Fatal("no probe ran against a backend without a Stager")
	}
}

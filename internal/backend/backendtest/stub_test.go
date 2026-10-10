package backendtest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// TestStubConformance runs the whole suite against Stub: the suite's own
// end-to-end test, and the proof that Stub keeps the contract.
func TestStubConformance(t *testing.T) {
	Run(t, NewStub(t).Target())
}

// stubWorkspace lays the fixture out in a fresh folder.
func stubWorkspace(t *testing.T, fx Fixture) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), FixtureRepo)
	if err := fx.Write(ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestStubRecordsCallsAndMounts(t *testing.T) {
	s := NewStub(t)
	ws := stubWorkspace(t, Fixture{})
	mounts := []backend.Mount{{LocalPath: "/host/x", ContainerPath: "/x"}}
	r, err := s.Up(ws, mounts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(r.ContainerID); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Container(r.ContainerID)
	if !ok || got.Status != backend.LiveStatusStopped || got.WorkspaceDir != ws || len(got.Mounts) != 1 || got.Mounts[0] != mounts[0] {
		t.Errorf("Container = %+v, %v", got, ok)
	}
	calls := s.Calls()
	if len(calls) != 2 || calls[0] != (StubCall{Method: "Up", WorkspaceDir: ws}) || calls[1] != (StubCall{Method: "Stop", ContainerID: r.ContainerID}) {
		t.Errorf("Calls = %+v", calls)
	}
}

func TestStubIgnoresMountsWithoutTheCapability(t *testing.T) {
	s := NewStub(t)
	s.Caps.SharedMounts = false
	r, err := s.Up(stubWorkspace(t, Fixture{}), []backend.Mount{{LocalPath: "/a", ContainerPath: "/b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Container(r.ContainerID); len(got.Mounts) != 0 || s.SupportsSharedMounts() {
		t.Errorf("mounts %v kept, SupportsSharedMounts %v", got.Mounts, s.SupportsSharedMounts())
	}
}

func TestStubFailOn(t *testing.T) {
	s := NewStub(t)
	boom := errors.New("boom")
	s.FailOn("Up", boom)
	if _, err := s.Up(stubWorkspace(t, Fixture{}), nil); !errors.Is(err, boom) {
		t.Errorf("Up error = %v, want boom", err)
	}
	if len(s.Containers()) != 0 {
		t.Errorf("a failed Up made a container")
	}
	s.FailOn("Up", nil)
	if _, err := s.Up(stubWorkspace(t, Fixture{}), nil); err != nil {
		t.Errorf("Up after clearing the failure: %v", err)
	}
}

func TestStubCapsGateCloneRebuildAndMic(t *testing.T) {
	s := NewStub(t)
	s.Caps = StubCaps{}
	ws := stubWorkspace(t, Fixture{})
	r, err := s.Up(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clone(r.ContainerID, t.TempDir(), nil); err == nil {
		t.Errorf("Clone succeeded without Caps.Clone")
	}
	if _, err := s.Rebuild(r.ContainerID, ws, nil); err == nil {
		t.Errorf("Rebuild succeeded without Caps.Rebuild")
	}
	if cmd, ok := s.MicSinkCommand(r.ContainerID); ok || cmd != nil {
		t.Errorf("MicSinkCommand offered without Caps.MicSink")
	}
}

func TestStubFailedLifecycleLeavesNoContainer(t *testing.T) {
	s := NewStub(t)
	_, err := s.Up(stubWorkspace(t, Fixture{PostCreate: "echo nope >&2; exit 3"}), nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Up error = %v, want the postCreateCommand's output", err)
	}
	if len(s.Containers()) != 0 {
		t.Errorf("a failed postCreateCommand left %v", s.Containers())
	}
}

func TestStubRefusesSudoAndOutsideCopies(t *testing.T) {
	s := NewStub(t)
	ws := stubWorkspace(t, Fixture{})
	if _, err := s.Up(ws, nil); err != nil {
		t.Fatal(err)
	}
	out, err := s.ExecCommand(ws, []string{"sudo", "true"}).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not available") {
		t.Errorf("sudo in a stub container: %v %s", err, out)
	}
	outside := filepath.Join(t.TempDir(), "escape")
	if err := s.CopyFile(ws, strings.NewReader("x"), outside, 0o644); err == nil {
		t.Errorf("CopyFile wrote outside the stub container")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("%s exists", outside)
	}
}

func TestStubCommandsSeeTheirOwnEnvironment(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("FLEET_CONFORMANCE_LEAK", "x")
	s := NewStub(t)
	ws := stubWorkspace(t, Fixture{})
	r, err := s.Up(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.ExecCommand(ws, ShArgv(`printf '%s|%s|%s|%s' "${TMUX-unset}" "${FLEET_CONFORMANCE_LEAK-unset}" "$HOME" "$TMUX_TMPDIR"`)).Output()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Container(r.ContainerID)
	parts := strings.Split(string(out), "|")
	if len(parts) != 4 || parts[0] != "unset" || parts[1] != "unset" || parts[2] != c.Home || parts[3] == "" {
		t.Errorf("container environment %q, want no TMUX, no test variables, HOME %s and a TMUX_TMPDIR", out, c.Home)
	}
}

func TestStubAgentToolProbe(t *testing.T) {
	s := NewStub(t)
	r, err := s.Up(stubWorkspace(t, Fixture{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tool, ok := s.AgentToolProbe(r.ContainerID); !ok || tool != "" {
		t.Errorf("probe = %q, %v", tool, ok)
	}
	s.SetAgentTool(r.ContainerID, "codex")
	if tool, ok := s.AgentToolProbe(r.ContainerID); !ok || tool != "codex" {
		t.Errorf("probe = %q, %v", tool, ok)
	}
	if err := s.Stop(r.ContainerID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.AgentToolProbe(r.ContainerID); ok {
		t.Errorf("probe succeeded on a stopped container")
	}
}

package fleetlaunch

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// recordingBackend is a backend.Backend that records the CopyFile transfer and
// the exec scripts run against it, succeeding every exec by shelling out to
// `true`. Only the methods the fleet-launch staging path touches do anything;
// the rest satisfy the interface.
type recordingBackend struct {
	copiedTo    string
	copiedBytes int
	copyMode    int
	execScripts []string
}

func (b *recordingBackend) CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error {
	data, _ := io.ReadAll(src)
	b.copiedBytes = len(data)
	b.copiedTo = remotePath
	b.copyMode = mode
	return nil
}

func (b *recordingBackend) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	if len(command) == 3 {
		b.execScripts = append(b.execScripts, command[2])
	}
	return backend.NewCmd(exec.Command("true"), nil)
}

func (b *recordingBackend) ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd {
	return b.ExecCommand(workspaceDir, command)
}

// --- interface filler: unused by the staging path ---

func (b *recordingBackend) Up(string, []backend.Mount) (*backend.UpResult, error) { return nil, nil }
func (b *recordingBackend) SupportsCustomMounts() bool                            { return false }
func (b *recordingBackend) Clone(string, string, []backend.Mount) (*backend.UpResult, error) {
	return nil, nil
}
func (b *recordingBackend) SupportsClone() bool { return false }
func (b *recordingBackend) Rebuild(string, string, []backend.Mount) (*backend.UpResult, error) {
	return nil, nil
}
func (b *recordingBackend) SupportsRebuild() bool       { return false }
func (b *recordingBackend) Down(string) error           { return nil }
func (b *recordingBackend) Stop(string) error           { return nil }
func (b *recordingBackend) Start(string) error          { return nil }
func (b *recordingBackend) Exec(string, []string) error { return nil }
func (b *recordingBackend) Stats([]string) (map[string]*backend.ContainerStats, error) {
	return nil, nil
}
func (b *recordingBackend) Logs(string, bool) error            { return nil }
func (b *recordingBackend) LogsCommand(string, bool) *exec.Cmd { return nil }
func (b *recordingBackend) CaptureAllSessions(string) backend.AllSessions {
	return backend.AllSessions{}
}
func (b *recordingBackend) ListSessions(string) string                    { return "" }
func (b *recordingBackend) AgentToolProbe(string) (string, bool)          { return "", false }
func (b *recordingBackend) RunScript(string, string) (string, error)      { return "", nil }
func (b *recordingBackend) EditorURI(string, string) (string, bool)       { return "", false }
func (b *recordingBackend) PortForwardCommand(string, int, int) *exec.Cmd { return nil }
func (b *recordingBackend) ForwardStdioCommand(string, int) (*exec.Cmd, bool) {
	return nil, false
}
func (b *recordingBackend) ResolveHostname(string) (string, bool) { return "", false }
func (b *recordingBackend) Status(string) backend.LiveStatus      { return backend.LiveStatusUnknown }

// TestCopyBinaryStagesViaCopyFileThenInstalls asserts the binary staging now
// goes through the backend's stdin-EOF-safe CopyFile (to a writable temp) and
// then installs into RemotePath with a stdin-free move — never a `cat >` that
// would hang on the coder backend.
func TestCopyBinaryStagesViaCopyFileThenInstalls(t *testing.T) {
	b := &recordingBackend{}
	if err := copyBinary(b, "/ws"); err != nil {
		t.Fatalf("copyBinary: %v", err)
	}

	if !strings.HasPrefix(b.copiedTo, "/tmp/.fleet-launch-stage.") {
		t.Errorf("staged to %q, want a unique /tmp/.fleet-launch-stage.* path", b.copiedTo)
	}
	if b.copyMode != 0o755 {
		t.Errorf("stage mode = %o, want 0755", b.copyMode)
	}
	if b.copiedBytes == 0 {
		t.Error("no bytes streamed to CopyFile")
	}
	if len(b.execScripts) != 1 {
		t.Fatalf("expected 1 install exec, got %d: %v", len(b.execScripts), b.execScripts)
	}
	install := b.execScripts[0]
	if !strings.Contains(install, b.copiedTo) || !strings.Contains(install, RemotePath) {
		t.Errorf("install script missing the staged/remote paths:\n%s", install)
	}
	if strings.Contains(install, "cat >") {
		t.Errorf("install must not stream over stdin (hangs on coder):\n%s", install)
	}
	if !strings.Contains(install, "sudo -n") {
		t.Errorf("install lost the sudo fallback for a non-writable /usr/bin:\n%s", install)
	}
}

// TestEnsureFleetRCWritesInlineThenWiresBashrc asserts the rc write is an inline
// (base64-in-argv) write — not a stdin `cat >` — and that the .bashrc wire still
// runs afterward.
func TestEnsureFleetRCWritesInlineThenWiresBashrc(t *testing.T) {
	b := &recordingBackend{}
	if err := EnsureFleetRC(b, "/ws", "/home/vscode", "inst-1"); err != nil {
		t.Fatalf("EnsureFleetRC: %v", err)
	}
	if len(b.execScripts) != 2 {
		t.Fatalf("expected 2 execs (inline write, bashrc wire), got %d", len(b.execScripts))
	}
	write := b.execScripts[0]
	if !strings.Contains(write, "base64 -d") || !strings.Contains(write, "/home/vscode/.fleet/fleet.rc") {
		t.Errorf("rc write is not an inline base64 write to the rc path:\n%s", write)
	}
	if strings.Contains(write, "cat >") {
		t.Errorf("rc write must not stream over stdin (hangs on coder):\n%s", write)
	}
	if wire := b.execScripts[1]; !strings.Contains(wire, ".bashrc") {
		t.Errorf("second exec should wire .bashrc:\n%s", wire)
	}
}

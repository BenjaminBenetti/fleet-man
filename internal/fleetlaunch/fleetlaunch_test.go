package fleetlaunch

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// recordingBackend is a backend.Backend that records the CopyFile transfer and
// the exec scripts run against it, returning configured probe responses. Only the methods the fleet-launch staging path touches do anything;
// the rest satisfy the interface.
type recordingBackend struct {
	copiedTo      string
	copiedBytes   int
	copyMode      int
	execScripts   []string
	failExec      bool // when set, every exec exits non-zero (drives error paths)
	failInstall   bool
	failPrepare   bool
	copyErr       error
	platform      string
	remoteVersion string
	stagedVersion string
	verifyErr     bool
	events        []string
}

func (b *recordingBackend) CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error {
	b.events = append(b.events, "copy")
	if b.copyErr != nil {
		return b.copyErr
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	b.copiedBytes = len(data)
	b.copiedTo = remotePath
	b.copyMode = mode
	return nil
}

func (b *recordingBackend) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	if len(command) == 3 {
		b.execScripts = append(b.execScripts, command[2])
	}
	output := ""
	fail := b.failExec
	switch {
	case len(command) == 2 && command[1] == "version":
		b.events = append(b.events, "verify")
		output, fail = b.stagedVersion, fail || b.verifyErr
	case len(command) == 3 && command[2] == "uname -s && uname -m":
		output = b.platform
		if output == "" {
			output = "Linux\nx86_64"
		}
	case len(command) == 3 && strings.Contains(command[2], "echo ABSENT"):
		output = b.remoteVersion
	case len(command) == 3 && strings.Contains(command[2], "chmod 0755"):
		b.events = append(b.events, "prepare")
		fail = fail || b.failPrepare
	case len(command) == 3 && strings.Contains(command[2], "mv -f") && !strings.Contains(command[2], "rm -f"):
		b.events = append(b.events, "install")
		fail = fail || b.failInstall
	}
	if fail {
		return backend.NewCmd(exec.Command("false"), nil)
	}
	return backend.NewCmd(exec.Command("printf", "%s", output), nil)
}

func (b *recordingBackend) ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd {
	return b.ExecCommand(workspaceDir, command)
}

// --- interface filler: unused by the staging path ---

func (b *recordingBackend) Up(string, []backend.Mount) (*backend.UpResult, error) { return nil, nil }
func (b *recordingBackend) SupportsSharedMounts() bool                            { return false }
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
func (b *recordingBackend) SupportsMicSink() bool                   { return false }
func (b *recordingBackend) MicSinkCommand(string) (*exec.Cmd, bool) { return nil, false }
func (b *recordingBackend) ResolveHostname(string) (string, bool)   { return "", false }
func (b *recordingBackend) Status(string) backend.LiveStatus        { return backend.LiveStatusUnknown }

// TestCopyBinaryStagesViaCopyFileThenInstalls asserts the binary staging now
// goes through the backend's stdin-EOF-safe CopyFile (to a writable temp) and
// then installs into RemotePath with a stdin-free move — never a `cat >` that
// would hang on the coder backend.
func TestCopyBinaryStagesViaCopyFileThenInstalls(t *testing.T) {
	b := &recordingBackend{}
	if err := copyBinary(b, "/ws", strings.NewReader("fleet binary"), nil); err != nil {
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
	if len(b.execScripts) != 2 {
		t.Fatalf("expected prepare + install execs, got %d: %v", len(b.execScripts), b.execScripts)
	}
	install := b.execScripts[1]
	if !strings.Contains(install, remoteDir+strings.TrimPrefix(b.copiedTo, "/tmp")) || !strings.Contains(install, RemotePath) {
		t.Errorf("install script missing the staged/remote paths:\n%s", install)
	}
	if strings.Contains(install, "rm -f") || strings.Contains(install, "chmod") {
		t.Fatalf("replacement must only rename: %s", install)
	}
	if !strings.Contains(b.execScripts[0], "sudo -n") {
		t.Fatal("prepare lost sudo fallback")
	}
	if strings.Contains(install, "cat >") {
		t.Errorf("install must not stream over stdin (hangs on coder):\n%s", install)
	}
	if !strings.Contains(install, "sudo -n") {
		t.Errorf("install lost the sudo fallback for a non-writable /usr/bin:\n%s", install)
	}
}

// TestCopyBinaryCleansStageOnInstallFailure asserts that when the install exec
// fails (e.g. /usr/bin not writable and sudo -n denied) the staged /tmp copy is
// removed best-effort, so retried provisions don't accumulate orphans.
func TestCopyBinaryCleansStageOnInstallFailure(t *testing.T) {
	b := &recordingBackend{failInstall: true}
	if err := copyBinary(b, "/ws", strings.NewReader("fleet binary"), nil); err == nil {
		t.Fatal("expected the failing install to surface as an error")
	}
	if len(b.execScripts) != 3 {
		t.Fatalf("expected prepare + install + cleanup execs, got %d: %v", len(b.execScripts), b.execScripts)
	}
	cleanup := b.execScripts[2]
	if !strings.Contains(cleanup, "rm -f") || !strings.Contains(cleanup, b.copiedTo) ||
		!strings.Contains(cleanup, remoteDir+strings.TrimPrefix(b.copiedTo, "/tmp")) || !strings.Contains(cleanup, "sudo -n") {
		t.Errorf("cleanup must rm the staged temp %q:\n%s", b.copiedTo, cleanup)
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

func fixtureArtifact(t *testing.T) *binstore.Artifact {
	t.Helper()
	data := []byte("resolved Linux fleet binary")
	path := filepath.Join(t.TempDir(), "fleet")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return &binstore.Artifact{Path: path, Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
}

func TestContainerArchitecture(t *testing.T) {
	for platform, want := range map[string]string{
		"Linux\nx86_64": "amd64", "Linux\namd64": "amd64",
		"Linux\naarch64": "arm64", "Linux\narm64": "arm64",
		"Darwin\narm64": "", "Linux\nriscv64": "", "garbage": "",
	} {
		t.Run(platform, func(t *testing.T) {
			got, err := containerArch(&recordingBackend{platform: platform}, "/ws")
			if got != want || (err != nil) != (want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
	if _, err := containerArch(&recordingBackend{failExec: true}, "/ws"); err == nil {
		t.Fatal("failed probe accepted")
	}
}

func TestEnsureFreshResolvesBeforeStoppingServices(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })
	for _, remote := range []string{"ABSENT", "NO_VERSION_CMD", "DEV", "VERSION v9.9.8", "unexpected", "VERSION v9.9.9"} {
		t.Run(remote, func(t *testing.T) {
			b := &recordingBackend{platform: "Linux\naarch64", remoteVersion: remote, stagedVersion: version.Version}
			resolve := func(arch string) (*binstore.Artifact, error) {
				if arch != "arm64" {
					t.Fatalf("resolved host architecture instead of instance: %s", arch)
				}
				b.events = append(b.events, "resolve")
				return fixtureArtifact(t), nil
			}
			changed, err := ensureFresh(b, "/ws", func() error { b.events = append(b.events, "stop"); return nil }, resolve)
			if err != nil {
				t.Fatal(err)
			}
			if remote == "VERSION v9.9.9" {
				if changed || len(b.events) != 0 {
					t.Fatalf("current binary churned: %v", b.events)
				}
			} else if !changed || strings.Join(b.events, ",") != "resolve,copy,prepare,verify,stop,install" {
				t.Fatalf("changed %v, events %v", changed, b.events)
			}
		})
	}
}

func TestEnsureFreshFailuresPreserveServices(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })
	for _, failure := range []string{"resolve", "open", "changed", "copy", "prepare", "verify", "version", "hook"} {
		t.Run(failure, func(t *testing.T) {
			b := &recordingBackend{remoteVersion: "VERSION v9.9.8", stagedVersion: version.Version}
			if failure == "copy" {
				b.copyErr = errors.New("copy failed")
			}
			if failure == "prepare" {
				b.failPrepare = true
			}
			if failure == "verify" {
				b.verifyErr = true
			}
			if failure == "version" {
				b.stagedVersion = "v9.9.8"
			}
			resolve := func(string) (*binstore.Artifact, error) {
				if failure == "resolve" {
					return nil, errors.New("unpublished release")
				}
				a := fixtureArtifact(t)
				if failure == "open" {
					if err := os.Remove(a.Path); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "changed" {
					if err := os.WriteFile(a.Path, []byte("changed"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				return a, nil
			}
			stopped := false
			changed, err := ensureFresh(b, "/ws", func() error { stopped = true; return errors.New("cannot stop") }, resolve)
			if err == nil || changed || stopped != (failure == "hook") {
				t.Fatalf("changed=%v stopped=%v err=%v", changed, stopped, err)
			}
			if strings.Contains(strings.Join(b.events, ","), "install") {
				t.Fatal("installed after failure")
			}
		})
	}
}

// localInstallBackend exercises the real copy/verify/install shell path inside
// a temporary directory, without requiring Docker or touching /usr/bin.
type localInstallBackend struct {
	recordingBackend
	dir    string
	direct bool
}

func (b *localInstallBackend) local(path string) string {
	if b.direct {
		return path
	}
	path = strings.ReplaceAll(path, remoteDir, b.dir+"/bin")
	return strings.ReplaceAll(path, "/tmp/.fleet-launch-stage.", b.dir+"/stage.")
}

func (b *localInstallBackend) CopyFile(_ string, src io.Reader, remotePath string, mode int) error {
	f, err := os.OpenFile(b.local(remotePath), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		return err
	}
	_, err = io.Copy(f, src)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (b *localInstallBackend) ExecCommand(_ string, args []string) *backend.Cmd {
	local := make([]string, len(args))
	for i, arg := range args {
		local[i] = b.local(arg)
	}
	return backend.NewCmd(exec.Command(local[0], local[1:]...), nil)
}

func TestCopyBinaryExecutesReplacementBeforeInstall(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })
	for _, failure := range []string{"", "execute", "version", "hook"} {
		t.Run(failure, func(t *testing.T) {
			b := &localInstallBackend{dir: t.TempDir()}
			if err := os.Mkdir(b.dir+"/bin", 0o755); err != nil {
				t.Fatal(err)
			}
			target := b.local(RemotePath)
			if err := os.WriteFile(target, []byte("old executable"), 0o755); err != nil {
				t.Fatal(err)
			}
			payload := "#!/bin/sh\nprintf 'v9.9.9\\n'\n"
			if failure == "execute" {
				payload = "not an executable"
			}
			if failure == "version" {
				payload = "#!/bin/sh\nprintf old"
			}
			oldFile, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			defer oldFile.Close()
			stopped := false
			err = copyBinary(b, "/ws", strings.NewReader(payload), func() error {
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "old executable" {
					t.Fatalf("old executable replaced before hook: %q %v", data, err)
				}
				stopped = true
				if failure == "hook" {
					return errors.New("hook failed")
				}
				return nil
			})
			if (err != nil) != (failure != "") || stopped != (failure == "" || failure == "hook") {
				t.Fatalf("failure=%s stopped=%v err=%v", failure, stopped, err)
			}
			data, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := payload
			if failure != "" {
				want = "old executable"
			}
			if string(data) != want {
				t.Fatalf("installed %q, want %q", data, want)
			}
			oldData, err := io.ReadAll(oldFile)
			if err != nil || string(oldData) != "old executable" {
				t.Fatalf("overwrote old inode: %q %v", oldData, err)
			}
			leftovers, err := filepath.Glob(b.dir + "/stage.*")
			siblings, siblingErr := filepath.Glob(b.dir + "/bin/.fleet-launch-stage.*")
			leftovers = append(leftovers, siblings...)
			if siblingErr != nil {
				t.Fatal(siblingErr)
			}
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("staging leftovers %v: %v", leftovers, err)
			}
		})
	}
}

func TestDevHostAlwaysRefreshes(t *testing.T) {
	old := version.Version
	version.Version = ""
	t.Cleanup(func() { version.Version = old })
	b := &recordingBackend{remoteVersion: "VERSION v9.9.9"}
	for range 2 {
		changed, err := ensureFresh(b, "/ws", nil, func(string) (*binstore.Artifact, error) { return fixtureArtifact(t), nil })
		if err != nil || !changed {
			t.Fatalf("dev refresh: %v %v", changed, err)
		}
	}
}

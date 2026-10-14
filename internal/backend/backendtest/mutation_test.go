package backendtest

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// recorder is a TB that records failures instead of failing the test, so a
// case can be run against a broken backend and expected to fail.
type recorder struct {
	t    *testing.T
	name string

	mu       sync.Mutex
	failed   bool
	skipped  bool
	messages []string
	cleanups []func()
}

func (r *recorder) Helper()      {}
func (r *recorder) Name() string { return r.name }

func (r *recorder) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *recorder) Logf(format string, args ...any) { r.note(format, args...) }

func (r *recorder) Errorf(format string, args ...any) {
	r.note("ERROR: "+format, args...)
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.note("SKIP: "+format, args...)
	r.mu.Lock()
	r.skipped = true
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

func (r *recorder) Cleanup(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, f)
}

func (r *recorder) TempDir() string {
	dir, err := os.MkdirTemp(r.t.TempDir(), "rec-")
	if err != nil {
		r.t.Fatal(err)
	}
	return dir
}

// do runs f as a test body would run: on its own goroutine, where Fatalf and
// Skipf end it, then the cleanups, last registered first, each able to fail.
func (r *recorder) do(f func()) {
	ran := func(f func()) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			f()
		}()
		<-done
	}
	ran(f)
	for {
		r.mu.Lock()
		if len(r.cleanups) == 0 {
			r.mu.Unlock()
			return
		}
		next := r.cleanups[len(r.cleanups)-1]
		r.cleanups = r.cleanups[:len(r.cleanups)-1]
		r.mu.Unlock()
		ran(next)
	}
}

// runCaseOn runs one case, with its own suite and patience, against b (stub
// underneath it answering Leftovers), recording how it went.
func runCaseOn(t *testing.T, stub *Stub, b backend.Backend, name string, patience float64) *recorder {
	t.Helper()
	tc, ok := caseByName(name)
	if !ok {
		t.Fatalf("no case %q", name)
	}
	target := stub.Target()
	target.Bind = func(*fleet.Instance) backend.Backend { return b }
	if caps, ok := b.(backend.Capabilities); ok {
		target.Capabilities = func(*fleet.Instance) backend.Capabilities { return caps }
	}
	rec := &recorder{t: t, name: name}
	rec.do(func() {
		s := newSuite(rec, &target)
		s.patience = patience
		s.runCase(rec, tc)
	})
	return rec
}

// wrap returns argv run through a shell prefix: `sh -c '<prefix> "$@"' sh argv...`.
func wrap(prefix string, argv []string) []string {
	return append([]string{"sh", "-c", prefix + ` "$@"`, "sh"}, argv...)
}

// Broken stubs, one fault each.
type (
	exitsZero         struct{ *Stub }
	mergesStderr      struct{ *Stub }
	dropsStdin        struct{ *Stub }
	corruptsOutput    struct{ *Stub }
	truncatesOutput   struct{ *Stub }
	ignoresCopyMode   struct{ *Stub }
	logsQuietExec     struct{ *Stub }
	asyncExec         struct{ *Stub }
	otherScriptUser   struct{ *Stub }
	wrongRemoteUser   struct{ *Stub }
	keepsDuplicates   struct{ *Stub }
	missingForEmpty   struct{ *Stub }
	micMismatch       struct{ *Stub }
	noStats           struct{ *Stub }
	followsLogs       struct{ *Stub }
	blindSessions     struct{ *Stub }
	unreachableHost   struct{ *Stub }
	wrongEditorFolder struct{ *Stub }
	postStartTwice    struct{ *Stub }
	stopLosesHome     struct{ *Stub }
	rebuildKeepsID    struct{ *Stub }
	shallowClone      struct{ *Stub }
	leaksFailedCreate struct{ *Stub }
	downLeaves        struct{ *Stub }
	noTTY             struct{ *Stub }
)

func (b exitsZero) ExecCommand(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommand(ws, wrap(`"$@"; exit 0; :`, argv))
}

func (b exitsZero) ExecCommandQuiet(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommandQuiet(ws, wrap(`"$@"; exit 0; :`, argv))
}

func (b mergesStderr) ExecCommand(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommand(ws, wrap(`exec 2>&1;`, argv))
}

func (b dropsStdin) ExecCommand(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommand(ws, wrap(`exec </dev/null;`, argv))
}

func (b corruptsOutput) ExecCommandQuiet(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommandQuiet(ws, append([]string{"sh", "-c", `"$@" | tr '\000' '\001'`, "sh"}, argv...))
}

func (b truncatesOutput) ExecCommandQuiet(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommandQuiet(ws, append([]string{"sh", "-c", `"$@" | head -c 1000`, "sh"}, argv...))
}

func (b ignoresCopyMode) CopyFile(ws string, src io.Reader, path string, _ int) error {
	return b.Stub.CopyFile(ws, src, path, 0o644)
}

func (b logsQuietExec) ExecCommandQuiet(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommand(ws, argv)
}

func (b asyncExec) Exec(ws string, argv []string) error {
	return b.Stub.ExecCommand(ws, argv).Start()
}

func (b otherScriptUser) RunScript(id, script string) (string, error) {
	return b.Stub.RunScript(id, `id() { echo 4242; }; `+script)
}

func (b wrongRemoteUser) Up(ws string, mounts []backend.Mount) (*backend.UpResult, error) {
	r, err := b.Stub.Up(ws, mounts)
	if r != nil {
		r.RemoteUser = "someone-else"
	}
	return r, err
}

func (b keepsDuplicates) Up(ws string, mounts []backend.Mount) (*backend.UpResult, error) {
	if _, running := b.forWorkspace(ws); !running {
		return b.Stub.Up(ws, mounts)
	}
	c, err := b.newContainer(ws, mounts)
	if err != nil {
		return nil, err
	}
	return &backend.UpResult{Outcome: "success", ContainerID: c.id, RemoteUser: b.user, RemoteWorkspaceFolder: ws}, nil
}

func (b missingForEmpty) Status(id string) backend.LiveStatus {
	if id == "" {
		return backend.LiveStatusMissing
	}
	return b.Stub.Status(id)
}

func (b micMismatch) MicSinkCommand(string) (*exec.Cmd, bool) { return nil, false }

func (b noStats) Stats([]string) (map[string]*backend.ContainerStats, error) {
	return map[string]*backend.ContainerStats{}, nil
}

func (b followsLogs) LogsCommand(id string, _ bool) *exec.Cmd { return b.Stub.LogsCommand(id, true) }

func (b blindSessions) ListSessions(string) string { return "" }

func (b unreachableHost) ResolveHostname(id string) (string, bool) {
	if _, ok := b.Stub.ResolveHostname(id); !ok {
		return "", false
	}
	return "192.0.2.1", true // TEST-NET-1: never answers
}

func (b wrongEditorFolder) EditorURI(ws, project string) (string, bool) {
	return "vscode-remote://stub+00/elsewhere", true
}

func (b postStartTwice) Start(id string) error {
	if err := b.Stub.Start(id); err != nil {
		return err
	}
	if err := b.Stub.Stop(id); err != nil {
		return err
	}
	return b.Stub.Start(id)
}

func (b stopLosesHome) Stop(id string) error {
	if c, ok := b.lookup(id); ok {
		_ = os.RemoveAll(c.home())
		_ = os.MkdirAll(c.home(), 0o700)
	}
	return b.Stub.Stop(id)
}

func (b rebuildKeepsID) Rebuild(id, ws string, mounts []backend.Mount) (*backend.UpResult, error) {
	r, err := b.Stub.Rebuild(id, ws, mounts)
	if r != nil {
		// Re-key the new container under the old ID.
		b.mu.Lock()
		c := b.containers[r.ContainerID]
		delete(b.containers, r.ContainerID)
		c.id = id
		b.containers[id] = c
		b.mu.Unlock()
		r.ContainerID = id
	}
	return r, err
}

func (b shallowClone) Clone(src, ws string, mounts []backend.Mount) (*backend.UpResult, error) {
	if _, ok := b.lookup(src); !ok {
		return nil, fmt.Errorf("no such container %s", src)
	}
	c, err := b.newContainer(ws, mounts)
	if err != nil {
		return nil, err
	}
	return &backend.UpResult{Outcome: "success", ContainerID: c.id, RemoteUser: b.user, RemoteWorkspaceFolder: ws}, nil
}

func (b leaksFailedCreate) Up(ws string, mounts []backend.Mount) (*backend.UpResult, error) {
	r, err := b.Stub.Up(ws, mounts)
	if err != nil {
		_, _ = b.newContainer(ws, mounts)
	}
	return r, err
}

func (b downLeaves) Down(id string) error {
	_ = b.begin("Down", id, "")
	return nil
}

func (b noTTY) ExecCommandQuiet(ws string, argv []string) *backend.Cmd {
	return b.Stub.ExecCommandQuiet(ws, wrap(`exec </dev/null >/dev/null 2>&1;`, argv))
}

// mutationPatience shrinks the cases' waits for the broken-backend runs and
// their control alike, so a case fails fast where it waits for a backend that
// never catches up, and the control proves the shrunk waits alone fail
// nothing.
const mutationPatience = 0.01

// TestCasesNoticeBrokenBackends runs each case against a Stub with one fault
// that case exists to catch, and requires the case to fail. Without it a case
// could pass by checking nothing.
func TestCasesNoticeBrokenBackends(t *testing.T) {
	_, noTmux := exec.LookPath("tmux")
	_, noSocat := exec.LookPath("socat")
	for _, tc := range []struct {
		caseName string
		fault    string
		broken   func(*Stub) backend.Backend
		missing  error // why a host tool the case needs on the stub is missing (nil: none needed, or present)
	}{
		{"up", "the UpResult names another user", func(s *Stub) backend.Backend { return wrongRemoteUser{s} }, nil},
		{"up-again", "a second Up leaves the first container running", func(s *Stub) backend.Backend { return keepsDuplicates{s} }, nil},
		{"capabilities", "MicSinkCommand disagrees with SupportsMicSink", func(s *Stub) backend.Backend { return micMismatch{s} }, nil},
		{"status", "an empty ContainerID reads as missing", func(s *Stub) backend.Backend { return missingForEmpty{s} }, nil},
		{"exec-exit-codes", "every command exits 0", func(s *Stub) backend.Backend { return exitsZero{s} }, nil},
		{"exec-streams", "stderr is merged into stdout", func(s *Stub) backend.Backend { return mergesStderr{s} }, nil},
		{"exec-stdin", "stdin never reaches the command", func(s *Stub) backend.Backend { return dropsStdin{s} }, nil},
		{"exec-user", "RunScript runs as another user", func(s *Stub) backend.Backend { return otherScriptUser{s} }, nil},
		{"exec-quiet", "ExecCommandQuiet logs", func(s *Stub) backend.Backend { return logsQuietExec{s} }, nil},
		{"exec-sync", "Exec returns before its command ends", func(s *Stub) backend.Backend { return asyncExec{s} }, nil},
		{"exec-tty", "the command gets no terminal", func(s *Stub) backend.Backend { return noTTY{s} }, nil},
		{"exec-tar", "output bytes are altered", func(s *Stub) backend.Backend { return corruptsOutput{s} }, nil},
		{"exec-pipe-out", "output is cut short", func(s *Stub) backend.Backend { return truncatesOutput{s} }, nil},
		{"copy-file", "CopyFile ignores the mode", func(s *Stub) backend.Backend { return ignoresCopyMode{s} }, nil},
		{"stats", "Stats reports nothing", func(s *Stub) backend.Backend { return noStats{s} }, nil},
		{"logs", "LogsCommand follows", func(s *Stub) backend.Backend { return followsLogs{s} }, nil},
		{"sessions", "ListSessions sees no sessions", func(s *Stub) backend.Backend { return blindSessions{s} }, noTmux},
		{"forward", "ResolveHostname offers an address nothing answers on", func(s *Stub) backend.Backend { return unreachableHost{s} }, noSocat},
		{"editor-uri", "the URI opens another folder", func(s *Stub) backend.Backend { return wrongEditorFolder{s} }, nil},
		{"stop-start", "Start runs postStartCommand twice", func(s *Stub) backend.Backend { return postStartTwice{s} }, nil},
		{"stop-start", "Stop loses $HOME", func(s *Stub) backend.Backend { return stopLosesHome{s} }, nil},
		{"rebuild", "Rebuild keeps the ContainerID", func(s *Stub) backend.Backend { return rebuildKeepsID{s} }, nil},
		{"rebuild-stopped", "Rebuild keeps the ContainerID", func(s *Stub) backend.Backend { return rebuildKeepsID{s} }, nil},
		{"clone", "Clone does not copy $HOME", func(s *Stub) backend.Backend { return shallowClone{s} }, nil},
		{"failed-create", "a failed create leaves its container", func(s *Stub) backend.Backend { return leaksFailedCreate{s} }, nil},
		{"up", "Down leaves the container", func(s *Stub) backend.Backend { return downLeaves{s} }, nil},
	} {
		t.Run(tc.caseName+"/"+strings.ReplaceAll(tc.fault, " ", "_"), func(t *testing.T) {
			if tc.missing != nil {
				t.Skipf("the stub run of %s needs a host tool: %v", tc.caseName, tc.missing)
			}
			stub := NewStub(t)
			rec := runCaseOn(t, stub, tc.broken(stub), tc.caseName, mutationPatience)
			if rec.skipped || !rec.Failed() {
				t.Errorf("%s passed against a backend where %s:\n%s", tc.caseName, tc.fault, strings.Join(rec.messages, "\n"))
				return
			}
			t.Logf("%s, as it should, failed:\n%s", tc.caseName, strings.Join(rec.messages, "\n"))
		})
	}
}

// TestCasesPassOnTheStub is the control for TestCasesNoticeBrokenBackends: each
// case, run the same way (recorder, patience) against the intact Stub, passes.
func TestCasesPassOnTheStub(t *testing.T) {
	for _, tc := range Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			stub := NewStub(t)
			rec := runCaseOn(t, stub, stub, tc.Name, mutationPatience)
			if rec.Failed() {
				t.Errorf("%s failed on the intact stub:\n%s", tc.Name, strings.Join(rec.messages, "\n"))
			}
		})
	}
}

package backendtest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// Stub is an in-memory backend.Backend for unit tests and for the conformance
// suite's own tests. It passes the suite: what it does, it does the way the
// contract says.
//
// Its containers are folders in a scratch directory (each with its own $HOME
// and TMPDIR), and a command sent into one runs as a process on this host:
// through sh, in the workspace folder, with an environment of the container's
// own (nothing of the test's environment but PATH, a TMUX_TMPDIR of its own
// and no TMUX, so a tmux started inside never reaches the caller's server). A
// sudo on the container's PATH refuses to run. It is a convenience, not a
// sandbox: a command naming an absolute path still reaches this host's file.
//
// Up and Start run the workspace's devcontainer.json postCreateCommand and
// postStartCommand (string or array forms; strict JSON); a failing one fails
// Up and leaves no container behind. Clone copies the source's $HOME. Stats
// are synthetic (StubMemoryMB of memory, no CPU), AgentToolProbe reports what
// SetAgentTool set, ResolveHostname is 127.0.0.1 (the host's loopback is the
// container's), and there is no stdio or listening port forwarder.
//
// Stub implements none of the optional backend capabilities (UpContexter,
// StreamExecer, OrphanDowner, ...); embed it in a type of the test's own to add
// one. Every method call is recorded (Calls), and FailOn makes a method fail.
type Stub struct {
	// Caps are the answers to the Supports* questions, and gate Clone,
	// Rebuild and MicSinkCommand. NewStub turns them all on. Set them before
	// the Stub is shared between goroutines.
	Caps StubCaps

	dir     string
	tmuxDir string
	user    string

	mu         sync.Mutex
	seq        int
	containers map[string]*stubContainer
	calls      []StubCall
	failures   map[string]error
	tools      map[string]string
}

// StubCaps are a Stub's capability answers.
type StubCaps struct {
	CustomMounts, Clone, Rebuild, MicSink bool
}

// StubCall is one recorded method call. ContainerID and WorkspaceDir are the
// arguments the method took ("" for one it did not).
type StubCall struct {
	Method       string
	ContainerID  string
	WorkspaceDir string
}

// StubContainer is a snapshot of one of a Stub's containers.
type StubContainer struct {
	ID           string
	WorkspaceDir string
	Status       backend.LiveStatus
	// Mounts are what Up, Clone or Rebuild was given (nil when
	// Caps.CustomMounts is off: the stub then ignores them).
	Mounts []backend.Mount
	// Home is the container's $HOME on this host.
	Home string
}

// StubMemoryMB is the memory Stats reports for each running container.
const StubMemoryMB = 64

type stubContainer struct {
	id        string
	workspace string
	running   bool
	mounts    []backend.Mount
	root      string // home, tmp, bin and the log live here
	tmux      string // TMUX_TMPDIR: short, since socket paths are limited
}

func (c *stubContainer) home() string    { return filepath.Join(c.root, "home") }
func (c *stubContainer) logPath() string { return filepath.Join(c.root, "container.log") }

// NewStub returns a Stub with every capability on, its containers in t's temp
// directories. Its containers' tmux servers are stopped when t ends.
func NewStub(t TB) *Stub {
	t.Helper()
	tmuxDir, err := os.MkdirTemp(shortTempRoot(), "bts-")
	if err != nil {
		t.Fatalf("stub tmux dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxDir) })
	user := "stub"
	if out, err := exec.Command("id", "-un").Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
		user = string(bytes.TrimSpace(out))
	}
	s := &Stub{
		Caps:       StubCaps{CustomMounts: true, Clone: true, Rebuild: true, MicSink: true},
		dir:        t.TempDir(),
		tmuxDir:    tmuxDir,
		user:       user,
		containers: map[string]*stubContainer{},
		failures:   map[string]error{},
		tools:      map[string]string{},
	}
	t.Cleanup(s.Close)
	return s
}

// shortTempRoot is where the tmux socket folders go: /tmp when there is one,
// since a unix socket path must stay under about 100 bytes and macOS's
// per-user temp folder is long.
func shortTempRoot() string {
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		return "/tmp"
	}
	return ""
}

// Target returns the conformance target running the suite against s.
func (s *Stub) Target() Target {
	return Target{
		Name:          "stub",
		Type:          fleet.BackendType("stub"),
		Bind:          func(*fleet.Instance) backend.Backend { return s },
		Capabilities:  func(*fleet.Instance) backend.Capabilities { return s },
		Leftovers:     s.leftovers,
		HostProcesses: true,
		EventLog:      flog.Path(),
		Sizes:         StubSizes(),
	}
}

// StubSizes keep the stub's run of the suite quick.
func StubSizes() Sizes {
	return Sizes{Tree: 4 << 20, Pipe: 3 << 20, Copy: 2 << 20}
}

// Close stops every container's tmux server. NewStub registers it with t.
func (s *Stub) Close() {
	s.mu.Lock()
	containers := make([]*stubContainer, 0, len(s.containers))
	for _, c := range s.containers {
		containers = append(containers, c)
	}
	s.mu.Unlock()
	for _, c := range containers {
		s.stopProcesses(c)
	}
}

// Calls returns the calls made so far, in order.
func (s *Stub) Calls() []StubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StubCall(nil), s.calls...)
}

// FailOn makes every later call of method (by name: "Up", "Down", "Stats",
// ...) fail with err without doing anything; a nil err clears it. Only the
// methods that return an error honour it.
func (s *Stub) FailOn(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.failures, method)
		return
	}
	s.failures[method] = err
}

// SetAgentTool sets what AgentToolProbe reports for a running container
// ("" for no agent).
func (s *Stub) SetAgentTool(containerID, tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[containerID] = tool
}

// Container returns a snapshot of the container with id.
func (s *Stub) Container(id string) (StubContainer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[id]
	if !ok {
		return StubContainer{}, false
	}
	return c.snapshot(), true
}

// Containers returns a snapshot of every container, by ID.
func (s *Stub) Containers() []StubContainer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubContainer, 0, len(s.containers))
	for _, c := range s.containers {
		out = append(out, c.snapshot())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (c *stubContainer) snapshot() StubContainer {
	status := backend.LiveStatusStopped
	if c.running {
		status = backend.LiveStatusRunning
	}
	return StubContainer{ID: c.id, WorkspaceDir: c.workspace, Status: status, Mounts: append([]backend.Mount(nil), c.mounts...), Home: c.home()}
}

// leftovers lists the containers made from inst's workspace.
func (s *Stub) leftovers(inst *fleet.Instance) ([]string, error) {
	var held []string
	for _, c := range s.Containers() {
		if c.WorkspaceDir == inst.WorkspaceDir {
			held = append(held, "container "+c.ID)
		}
	}
	return held, nil
}

// begin records a call and returns the failure FailOn set for it.
func (s *Stub) begin(method, containerID, workspaceDir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, StubCall{Method: method, ContainerID: containerID, WorkspaceDir: workspaceDir})
	return s.failures[method]
}

// lookup returns the container with id.
func (s *Stub) lookup(id string) (*stubContainer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[id]
	return c, ok
}

// running returns the container with id when it is running.
func (s *Stub) running(id string) (*stubContainer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[id]
	return c, ok && c.running
}

// forWorkspace returns the container made from workspaceDir, and whether it
// is running.
func (s *Stub) forWorkspace(workspaceDir string) (*stubContainer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *stubContainer
	for _, c := range s.containers {
		if c.workspace == workspaceDir && (found == nil || c.running) {
			found = c
		}
	}
	return found, found != nil && found.running
}

// newContainer makes a running container for workspaceDir.
func (s *Stub) newContainer(workspaceDir string, mounts []backend.Mount) (*stubContainer, error) {
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("stub-%d-%s", s.seq, randHex(4))
	c := &stubContainer{
		id:        id,
		workspace: workspaceDir,
		running:   true,
		root:      filepath.Join(s.dir, id),
		tmux:      filepath.Join(s.tmuxDir, fmt.Sprint(s.seq)),
	}
	if s.Caps.CustomMounts {
		c.mounts = append([]backend.Mount(nil), mounts...)
	}
	s.mu.Unlock()
	for _, dir := range []string{c.home(), filepath.Join(c.root, "tmp"), filepath.Join(c.root, "bin"), c.tmux} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(c.root, "bin", "sudo"), []byte(stubSudo), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.logPath(), nil, 0o600); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.containers[id] = c
	s.mu.Unlock()
	return c, nil
}

// stubSudo stands in for sudo inside a stub container: commands there run on
// this host, so nothing may gain root through one.
const stubSudo = "#!/bin/sh\necho 'sudo: not available in a backendtest stub container' >&2\nexit 1\n"

// remove stops c's processes and forgets it.
func (s *Stub) remove(c *stubContainer) {
	s.stopProcesses(c)
	s.mu.Lock()
	delete(s.containers, c.id)
	delete(s.tools, c.id)
	s.mu.Unlock()
	_ = os.RemoveAll(c.root)
}

// removeWorkspace removes every container made from workspaceDir: Up's prune.
func (s *Stub) removeWorkspace(workspaceDir string) {
	s.mu.Lock()
	var stale []*stubContainer
	for _, c := range s.containers {
		if c.workspace == workspaceDir {
			stale = append(stale, c)
		}
	}
	s.mu.Unlock()
	for _, c := range stale {
		s.remove(c)
	}
}

// stopProcesses ends c's tmux server, which is all a stub container keeps
// running between commands. The server is named by its socket path, never
// found through the environment, so no other tmux server can be hit.
func (s *Stub) stopProcesses(c *stubContainer) {
	socket := filepath.Join(c.tmux, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if _, err := os.Stat(socket); err != nil {
		return
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return
	}
	_ = s.command(c, c.home(), []string{"tmux", "-S", socket, "kill-server"}).Run()
}

// up creates a running container for workspaceDir and runs its lifecycle
// commands; a failing one removes the container again.
func (s *Stub) up(workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	if info, err := os.Stat(workspaceDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("stub up: workspace %q is not a folder", workspaceDir)
	}
	lc, err := readStubLifecycle(workspaceDir)
	if err != nil {
		return nil, err
	}
	s.removeWorkspace(workspaceDir)
	c, err := s.newContainer(workspaceDir, mounts)
	if err != nil {
		return nil, err
	}
	for _, step := range []struct {
		name string
		argv []string
	}{{"postCreateCommand", lc.postCreate}, {"postStartCommand", lc.postStart}} {
		if err := s.runLifecycle(c, step.name, step.argv); err != nil {
			s.remove(c)
			return nil, err
		}
	}
	return &backend.UpResult{Outcome: "success", ContainerID: c.id, RemoteUser: s.user, RemoteWorkspaceFolder: workspaceDir}, nil
}

// Up creates a container for workspaceDir, first removing any made from it
// before.
func (s *Stub) Up(workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	if err := s.begin("Up", "", workspaceDir); err != nil {
		return nil, err
	}
	return s.up(workspaceDir, mounts)
}

// SupportsCustomMounts reports Caps.CustomMounts.
func (s *Stub) SupportsCustomMounts() bool { return s.Caps.CustomMounts }

// Clone makes a running container for destWorkspaceDir with a copy of the
// source container's $HOME. The caller copies the workspace, as with
// devcontainer.
func (s *Stub) Clone(sourceContainerID, destWorkspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	if err := s.begin("Clone", sourceContainerID, destWorkspaceDir); err != nil {
		return nil, err
	}
	if !s.Caps.Clone {
		return nil, errors.New("stub: clone is not supported")
	}
	src, ok := s.lookup(sourceContainerID)
	if !ok {
		return nil, fmt.Errorf("stub clone: no such container %s", sourceContainerID)
	}
	if info, err := os.Stat(destWorkspaceDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("stub clone: workspace %q is not a folder", destWorkspaceDir)
	}
	s.removeWorkspace(destWorkspaceDir)
	c, err := s.newContainer(destWorkspaceDir, mounts)
	if err != nil {
		return nil, err
	}
	if out, err := exec.Command("cp", "-a", src.home()+"/.", c.home()+"/").CombinedOutput(); err != nil {
		s.remove(c)
		return nil, fmt.Errorf("stub clone: copy $HOME: %v\n%s", err, out)
	}
	return &backend.UpResult{Outcome: "success", ContainerID: c.id, RemoteUser: s.user, RemoteWorkspaceFolder: destWorkspaceDir}, nil
}

// SupportsClone reports Caps.Clone.
func (s *Stub) SupportsClone() bool { return s.Caps.Clone }

// Rebuild replaces the workspace's container with a fresh one (a new ID and
// an empty $HOME), as devcontainer's does.
func (s *Stub) Rebuild(containerID, workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	if err := s.begin("Rebuild", containerID, workspaceDir); err != nil {
		return nil, err
	}
	if !s.Caps.Rebuild {
		return nil, errors.New("stub: rebuild is not supported")
	}
	return s.up(workspaceDir, mounts)
}

// SupportsRebuild reports Caps.Rebuild.
func (s *Stub) SupportsRebuild() bool { return s.Caps.Rebuild }

// Down removes the container.
func (s *Stub) Down(containerID string) error {
	if err := s.begin("Down", containerID, ""); err != nil {
		return err
	}
	c, ok := s.lookup(containerID)
	if !ok {
		return fmt.Errorf("stub down: no such container %s", containerID)
	}
	s.remove(c)
	return nil
}

// Stop stops the container's processes and marks it stopped.
func (s *Stub) Stop(containerID string) error {
	if err := s.begin("Stop", containerID, ""); err != nil {
		return err
	}
	c, ok := s.lookup(containerID)
	if !ok {
		return fmt.Errorf("stub stop: no such container %s", containerID)
	}
	s.stopProcesses(c)
	s.mu.Lock()
	c.running = false
	s.mu.Unlock()
	return nil
}

// Start marks a stopped container running and runs postStartCommand, best
// effort (a failure goes to the container's log), as devcontainer's does.
func (s *Stub) Start(containerID string) error {
	if err := s.begin("Start", containerID, ""); err != nil {
		return err
	}
	c, ok := s.lookup(containerID)
	if !ok {
		return fmt.Errorf("stub start: no such container %s", containerID)
	}
	s.mu.Lock()
	wasRunning := c.running
	c.running = true
	s.mu.Unlock()
	if wasRunning {
		return nil
	}
	if lc, err := readStubLifecycle(c.workspace); err == nil {
		_ = s.runLifecycle(c, "postStartCommand", lc.postStart)
	}
	return nil
}

// Status reports the container's state: unknown for an empty ID, missing for
// one it does not have.
func (s *Stub) Status(containerID string) backend.LiveStatus {
	_ = s.begin("Status", containerID, "")
	if containerID == "" {
		return backend.LiveStatusUnknown
	}
	c, ok := s.lookup(containerID)
	switch {
	case !ok:
		return backend.LiveStatusMissing
	case s.isRunning(c):
		return backend.LiveStatusRunning
	default:
		return backend.LiveStatusStopped
	}
}

func (s *Stub) isRunning(c *stubContainer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.running
}

// EditorURI is a vscode-remote URI naming the stub and the workspace.
func (s *Stub) EditorURI(workspaceDir, _ string) (string, bool) {
	_ = s.begin("EditorURI", "", workspaceDir)
	return "vscode-remote://stub+" + hex.EncodeToString([]byte(workspaceDir)) + workspaceDir, true
}

// ResolveHostname is the host's loopback while the container runs: its
// processes are this host's.
func (s *Stub) ResolveHostname(containerID string) (string, bool) {
	_ = s.begin("ResolveHostname", containerID, "")
	if _, ok := s.running(containerID); !ok {
		return "", false
	}
	return "127.0.0.1", true
}

// PortForwardCommand returns a command that fails: the stub has no listening
// forwarder (ResolveHostname's address is dialed directly).
func (s *Stub) PortForwardCommand(containerID string, _, _ int) *exec.Cmd {
	_ = s.begin("PortForwardCommand", containerID, "")
	return failing("stub: no port forwarder; dial the ResolveHostname address")
}

// ForwardStdioCommand reports no stdio bridge.
func (s *Stub) ForwardStdioCommand(containerID string, _ int) (*exec.Cmd, bool) {
	_ = s.begin("ForwardStdioCommand", containerID, "")
	return nil, false
}

// SupportsMicSink reports Caps.MicSink.
func (s *Stub) SupportsMicSink() bool { return s.Caps.MicSink }

// MicSinkCommand returns a sink that swallows its stdin, when Caps.MicSink.
func (s *Stub) MicSinkCommand(containerID string) (*exec.Cmd, bool) {
	_ = s.begin("MicSinkCommand", containerID, "")
	if !s.Caps.MicSink {
		return nil, false
	}
	c, ok := s.running(containerID)
	if !ok {
		return failing("stub: no running container " + containerID), true
	}
	return s.command(c, c.home(), []string{"sh", "-c", "cat >/dev/null"}), true
}

// AgentToolProbe reports what SetAgentTool set for a running container.
func (s *Stub) AgentToolProbe(containerID string) (string, bool) {
	_ = s.begin("AgentToolProbe", containerID, "")
	if _, ok := s.running(containerID); !ok {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tools[containerID], true
}

// Stats reports StubMemoryMB for each running container asked about.
func (s *Stub) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	if err := s.begin("Stats", strings.Join(containerIDs, ","), ""); err != nil {
		return nil, err
	}
	out := map[string]*backend.ContainerStats{}
	for _, id := range containerIDs {
		if _, ok := s.running(id); ok {
			out[id] = &backend.ContainerStats{MemoryMB: StubMemoryMB}
		}
	}
	return out, nil
}

var _ backend.Backend = (*Stub)(nil)

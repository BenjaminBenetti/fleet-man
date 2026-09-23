package server

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"golang.org/x/crypto/ssh/agent"
)

// sshagent_linux_test.go: the per-instance relay sockets exist only on Linux
// (agentsock.ModeRelay); on macOS instances use Docker Desktop's agent.

func TestSSHAgentInstanceSocketsFollowState(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)

	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"f": {Name: "f", Instances: []*fleet.Instance{
			{Name: "i", Backend: fleet.BackendDevcontainer, Status: fleet.StatusRunning},
			{Name: "nocontrol", Backend: fleet.BackendCoder, Status: fleet.StatusRunning},
		}},
	}}
	if err := os.MkdirAll(state.ControlDir("f", "i"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st)

	sock := filepath.Join(state.ControlDir("f", "i"), agentsock.SocketName)
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("instance socket not created: %v", err)
	}
	// 0666 so a container user with another uid can connect; the peer check
	// (daemon user, root, or that instance's container) decides who is served.
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
		t.Fatalf("instance socket mode = %v, want a 0666 socket", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(state.ControlDir("f", "nocontrol"), agentsock.SocketName)); err == nil {
		t.Fatal("an instance without a control directory must not get a socket")
	}

	provider := attachRawProvider(t, client, "laptop")
	provider.nextStatus()
	dialAsync(t, sock)
	if origin := provider.nextOpen().GetOrigin(); origin != "f/i" {
		t.Fatalf("origin = %q, want f/i", origin)
	}

	// The instance is destroyed: its socket goes with it.
	svc.agent.syncInstances(&state.State{Fleets: map[string]*fleet.Fleet{}})
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be removed with its instance, stat err = %v", err)
	}
}

func TestSSHAgentInstanceSocketWithLongNames(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)

	// Far past the 108-byte unix socket path limit.
	fleetName := "a-rather-long-fleet-name-for-the-platform-backend-service"
	instName := "feature-auth-refactor-with-a-long-descriptive-instance-name"
	control := state.ControlDir(fleetName, instName)
	if len(control) < 110 {
		t.Fatalf("test setup: control dir %d bytes, want it past the socket limit", len(control))
	}
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.ensureInstance(fleetName, instName)
	sock := filepath.Join(control, agentsock.SocketName)
	if info, err := os.Lstat(sock); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no socket at a long path: %v", err)
	}

	provider := attachRawProvider(t, client, "laptop")
	provider.nextStatus()
	// Connect the way a container does, through a short path to the same
	// directory (a symlink standing in for the /fleet-mounts/control mount).
	short := filepath.Join(dir, "m")
	if err := os.Symlink(control, short); err != nil {
		t.Fatal(err)
	}
	dialAsync(t, filepath.Join(short, agentsock.SocketName))
	if origin := provider.nextOpen().GetOrigin(); origin != fleetName+"/"+instName {
		t.Fatalf("origin = %q", origin)
	}
}

func TestSSHAgentInstanceSocketNeverFollowsASymlink(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)

	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	// A process in the instance plants a symlink where the socket goes,
	// pointing at a host file it wants the daemon to chmod.
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(control, agentsock.SocketName)); err != nil {
		t.Fatal(err)
	}
	svc.agent.ensureInstance("f", "i")
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the symlink target's mode changed: %v %v", info.Mode(), err)
	}
	if info, err := os.Lstat(filepath.Join(control, agentsock.SocketName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the planted entry should be left alone (and no socket served): %v", err)
	}
}

func TestSSHAgentInstanceSocketIsRecreatedWhenDeleted(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"f": {Name: "f", Instances: []*fleet.Instance{{Name: "i", Backend: fleet.BackendDevcontainer}}},
	}}
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st)
	sock := filepath.Join(control, agentsock.SocketName)
	// The instance is destroyed and re-created under the same name between
	// two reconciles: the old listener is bound to nothing.
	if err := os.RemoveAll(control); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st) // notices the stale listener
	svc.agent.syncInstances(st) // opens a fresh one
	if info, err := os.Lstat(sock); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket not recreated: %v", err)
	}
}

func TestSSHAgentFallbackFiltersInstanceConnections(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")
	svc, _ := startAgentTestServer(t)
	svc.agent.setFallback(daemonSock)

	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.ensureInstance("f", "i")
	instSock := filepath.Join(control, agentsock.SocketName)

	conn, err := net.Dial("unix", instSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agent.NewClient(conn)
	if err := client.RemoveAll(); err == nil {
		t.Fatal("an instance must not be able to remove the daemon agent's keys")
	}
	if err := client.Lock([]byte("x")); err == nil {
		t.Fatal("an instance must not be able to lock the daemon's agent")
	}
	keys, err := client.List()
	if err != nil || len(keys) != 1 || string(keys[0].Marshal()) != string(daemonKey.Marshal()) {
		t.Fatalf("listing still works and the key is intact: %v %v", keys, err)
	}
	sig, err := client.Sign(daemonKey, []byte("d"))
	if err != nil || daemonKey.Verify([]byte("d"), sig) != nil {
		t.Fatalf("signing through the filtered fallback: %v", err)
	}
}

func TestInstanceSocketRefusesASocketSwappedForASymlinkWhileBinding(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A process in the instance wins the race: between bind and chmod it
	// moves the fresh socket away and puts a symlink to a host file there.
	afterInstanceBind = func(d, name string) {
		_ = os.Rename(filepath.Join(d, name), filepath.Join(d, "moved"))
		_ = os.Symlink(target, filepath.Join(d, name))
	}
	t.Cleanup(func() { afterInstanceBind = nil })

	svc.agent.ensureInstance("f", "i")
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the daemon changed the symlink target's mode: %v %v", info.Mode(), err)
	}
	svc.agent.mu.Lock()
	_, listening := svc.agent.instances["f/i"]
	svc.agent.mu.Unlock()
	if listening {
		t.Fatal("the listen must fail when the socket was replaced while binding")
	}
}

func TestInstanceSocketReplacesAStaleSocket(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(control, agentsock.SocketName)
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Skipf("control path too long for a direct bind here: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close() // a crashed daemon's leftover
	svc.agent.ensureInstance("f", "i")
	conn, err := net.Dial("unix", stale)
	if err != nil {
		t.Fatalf("the stale socket was not replaced by a live one: %v", err)
	}
	_ = conn.Close()
}

func TestSSHAgentInstanceSocketIsRecreatedWhenReplaced(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"f": {Name: "f", Instances: []*fleet.Instance{{Name: "i", Backend: fleet.BackendDevcontainer}}},
	}}
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st)
	sock := filepath.Join(control, agentsock.SocketName)
	// A process in the instance swaps in a socket of its own.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	impostor, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("control path too long for a direct bind here: %v", err)
	}
	impostor.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = impostor.Close()

	svc.agent.syncInstances(st) // notices the listener no longer owns the path
	svc.agent.syncInstances(st) // opens a fresh one
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("the relay socket was not recreated after being replaced: %v", err)
	}
	_ = conn.Close()
}

// TestEnsureInstanceWaitsForAnOpenInFlight: the provisioning hook promises
// the instance's socket exists when it returns — also when a reconcile is
// opening the same socket at that moment, and when that open fails.
func TestEnsureInstanceWaitsForAnOpenInFlight(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	t.Setenv(agentsock.EnvOverride, "")
	h := newAgentHub()
	t.Cleanup(h.close)
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}

	inflight := make(chan struct{}) // a reconcile's open of f/i
	h.mu.Lock()
	h.opening["f/i"] = inflight
	h.mu.Unlock()
	returned := make(chan struct{})
	go func() {
		h.ensureInstance("f", "i")
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("the hook returned while another open of its socket was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	// That open settles without a socket (it failed): the hook opens one.
	h.mu.Lock()
	delete(h.opening, "f/i")
	close(inflight)
	h.mu.Unlock()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never returned")
	}
	if info, err := os.Lstat(filepath.Join(control, agentsock.SocketName)); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no instance socket once the hook returned: %v", err)
	}
}

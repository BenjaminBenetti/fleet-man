package server

import (
	"os"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// openEvent is one decoded browser.open the registry forwarded via onOpen.
type openEvent struct {
	fleet, instance, url string
}

// copyEvent is one decoded file.copy the registry forwarded via onCopy.
type copyEvent struct {
	fleet, instance, src, dst string
	open                      bool
}

// shortHome creates a temp HOME under /tmp and points $HOME at it. Not
// t.TempDir(): the control socket lives at
// $HOME/.fleet/workspaces/<fleet>/<inst>/.control/fleet.sock, and on macOS
// the /var/folders/... test temp dirs push that past the platform's
// 104-byte sun_path limit, so the listener never binds.
func shortHome(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fmctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("HOME", dir)
}

// waitForSocket polls path until it exists or the deadline passes.
func waitForSocket(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// fabricatedRunningState builds a *state.State with one running instance.
func fabricatedRunningState(fleetName, instanceName string) *state.State {
	inst := &fleet.Instance{Name: instanceName, Status: fleet.StatusRunning}
	f := &fleet.Fleet{Name: fleetName, Instances: []*fleet.Instance{inst}}
	return &state.State{Fleets: map[string]*fleet.Fleet{fleetName: f}}
}

// TestControlSyncRoundTrip verifies the server path: syncRunning starts a
// listener for a running instance (its socket appears), an in-instance
// client.OpenBrowser round-trips an event into onOpen tagged with the right
// fleet/instance, and a stopped instance's listener is dropped on the next sync.
func TestControlSyncRoundTrip(t *testing.T) {
	shortHome(t)

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)
	key := fleetName + "/" + instanceName

	events := make(chan openEvent, 16)
	copies := make(chan copyEvent, 16)
	r := newControlRegistry(func(f, i, url string) {
		select {
		case events <- openEvent{f, i, url}:
		default:
		}
	}, func(f, i, src, dst string, open bool) {
		select {
		case copies <- copyEvent{f, i, src, dst, open}:
		default:
		}
	}, nil)
	defer r.shutdown()

	r.syncRunning(fabricatedRunningState(fleetName, instanceName))

	socketPath := state.ControlSocketPath(fleetName, instanceName)
	if !waitForSocket(t, socketPath, 2*time.Second) {
		t.Fatalf("control socket %q never appeared after syncRunning", socketPath)
	}
	if _, ok := r.servers[key]; !ok {
		t.Fatalf("registry has no server for running instance %q", key)
	}

	client, err := control.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial(%q): %v", socketPath, err)
	}
	defer client.Close()

	const wantURL = "http://localhost:3000"
	if err := client.OpenBrowser(wantURL); err != nil {
		t.Fatalf("OpenBrowser: %v", err)
	}

	select {
	case ev := <-events:
		if ev.fleet != fleetName || ev.instance != instanceName {
			t.Errorf("event = (%q,%q), want (%q,%q)", ev.fleet, ev.instance, fleetName, instanceName)
		}
		if ev.url != wantURL {
			t.Errorf("event URL = %q, want %q", ev.url, wantURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser.open on onOpen")
	}

	// A file.copy envelope on the same socket dispatches to onCopy, carrying
	// the two endpoints and the `fleet open` flag through verbatim.
	const (
		wantSrc = ":bin/tool"
		wantDst = "~/builds/tool"
	)
	for _, wantOpen := range []bool{false, true} {
		if err := client.CopyFile(wantSrc, wantDst, wantOpen); err != nil {
			t.Fatalf("CopyFile(open=%v): %v", wantOpen, err)
		}
		select {
		case ev := <-copies:
			if ev.fleet != fleetName || ev.instance != instanceName {
				t.Errorf("copy event = (%q,%q), want (%q,%q)", ev.fleet, ev.instance, fleetName, instanceName)
			}
			if ev.src != wantSrc || ev.dst != wantDst || ev.open != wantOpen {
				t.Errorf("copy event = (%q,%q,open=%v), want (%q,%q,open=%v)", ev.src, ev.dst, ev.open, wantSrc, wantDst, wantOpen)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for file.copy(open=%v) on onCopy", wantOpen)
		}
	}

	r.syncRunning(&state.State{Fleets: map[string]*fleet.Fleet{}})
	if _, ok := r.servers[key]; ok {
		t.Fatalf("registry still has server for stopped instance %q", key)
	}
}

// TestControlShutdownFullBuffer guards against a blocked-send deadlock: the
// per-instance handler invokes onOpen inside control.Server.serveConn, whose
// goroutine Close waits on. A flood of browser.open messages with a non-draining
// onOpen must not wedge serveConn, so shutdown() returns promptly. (The test's
// onOpen drops on a full buffer, mirroring the server's non-blocking hub.post.)
func TestControlShutdownFullBuffer(t *testing.T) {
	shortHome(t)

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)

	events := make(chan openEvent, 16) // small, never drained
	r := newControlRegistry(func(f, i, url string) {
		select {
		case events <- openEvent{f, i, url}:
		default:
		}
	}, nil, nil)

	r.syncRunning(fabricatedRunningState(fleetName, instanceName))
	socketPath := state.ControlSocketPath(fleetName, instanceName)
	if !waitForSocket(t, socketPath, 2*time.Second) {
		t.Fatalf("control socket %q never appeared", socketPath)
	}

	client, err := control.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial(%q): %v", socketPath, err)
	}
	defer client.Close()

	for i := 0; i < 256; i++ {
		if err := client.OpenBrowser("http://localhost:3000"); err != nil {
			t.Fatalf("OpenBrowser #%d: %v", i, err)
		}
	}

	done := make(chan struct{})
	go func() {
		r.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown() hung with a full, undrained onOpen (blocked-send deadlock)")
	}
}

// TestControlSyncIdempotent verifies repeated syncs against an unchanged running
// set keep the same server pointer (no leak, no recreate).
func TestControlSyncIdempotent(t *testing.T) {
	shortHome(t)

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)
	key := fleetName + "/" + instanceName

	r := newControlRegistry(func(string, string, string) {}, nil, nil)
	defer r.shutdown()

	st := fabricatedRunningState(fleetName, instanceName)
	r.syncRunning(st)
	first := r.servers[key]
	if first == nil {
		t.Fatalf("no server after first sync")
	}
	r.syncRunning(st)
	if second := r.servers[key]; second != first {
		t.Fatalf("second sync replaced the server pointer (%p -> %p)", first, second)
	}
	if len(r.servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(r.servers))
	}
}

// TestCtrlGate verifies the subscriber gate: with no browser-capable client
// attached, a tick tears every control socket down so an in-container `fleet
// launch` reports "not connected to host fleet" rather than succeeding into the
// void (integration test 510). (Short name keeps the temp-HOME socket path under
// the unix sun_path limit.)
func TestCtrlGate(t *testing.T) {
	shortHome(t)

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)
	key := fleetName + "/" + instanceName

	attached := true
	r := newControlRegistry(func(string, string, string) {}, nil, func() bool { return attached })
	defer r.shutdown()

	r.syncRunning(fabricatedRunningState(fleetName, instanceName))
	socketPath := state.ControlSocketPath(fleetName, instanceName)
	if !waitForSocket(t, socketPath, 2*time.Second) {
		t.Fatalf("control socket %q never appeared", socketPath)
	}

	// Client detaches → the next tick must drop every listener + socket file.
	attached = false
	r.tick()
	if _, ok := r.servers[key]; ok {
		t.Fatalf("control server for %q still present after the gate closed", key)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("control socket %q still exists after the gate closed", socketPath)
	}
}

// TestSplitInstanceKey covers the "<fleet>/<instance>" splitter.
func TestSplitInstanceKey(t *testing.T) {
	cases := []struct {
		key          string
		wantFleet    string
		wantInstance string
		wantOK       bool
	}{
		{"alpha/i1", "alpha", "i1", true},
		{"a/b/c", "a", "b/c", true},
		{"noseparator", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			f, i, ok := splitInstanceKey(tc.key)
			if f != tc.wantFleet || i != tc.wantInstance || ok != tc.wantOK {
				t.Errorf("splitInstanceKey(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.key, f, i, ok, tc.wantFleet, tc.wantInstance, tc.wantOK)
			}
		})
	}
}

package tui

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// waitForSocket polls path until it exists or the deadline passes. The control
// listener creates the socket file synchronously inside Listen, but syncRunning
// runs it best-effort, so the test waits rather than assuming instantaneous
// appearance on a loaded machine.
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

// fabricatedRunningState builds a *state.State containing one running instance
// in one fleet, the minimal shape syncRunning keys its listeners off.
func fabricatedRunningState(fleetName, instanceName string) *state.State {
	inst := &fleet.Instance{Name: instanceName, Status: fleet.StatusRunning}
	f := &fleet.Fleet{Name: fleetName, Instances: []*fleet.Instance{inst}}
	return &state.State{Fleets: map[string]*fleet.Fleet{fleetName: f}}
}

// TestSyncRunningRoundTrip verifies the full host-side path: syncRunning starts
// a listener for a running instance (its socket file appears), an in-instance
// client.OpenBrowser round-trips an event onto the registry's events channel
// tagged with the right instance key, and a stopped instance's listener is
// dropped on the next sync.
func TestSyncRunningRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)
	key := fleetName + "/" + instanceName

	r := newControlRegistry()
	defer r.shutdown()

	// 1. Start a listener for the running instance.
	r.syncRunning(fabricatedRunningState(fleetName, instanceName))

	socketPath := state.ControlSocketPath(fleetName, instanceName)
	if !waitForSocket(t, socketPath, 2*time.Second) {
		t.Fatalf("control socket %q never appeared after syncRunning", socketPath)
	}
	if _, ok := r.servers[key]; !ok {
		t.Fatalf("registry has no server for running instance %q", key)
	}

	// 2. Dial as the in-instance client would and send a browser.open.
	client, err := control.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial(%q): %v", socketPath, err)
	}
	defer client.Close()

	const wantURL = "http://localhost:3000"
	if err := client.OpenBrowser(wantURL); err != nil {
		t.Fatalf("OpenBrowser: %v", err)
	}

	// 3. The event arrives on the registry channel, correctly tagged.
	select {
	case ev := <-r.events:
		if ev.instanceKey != key {
			t.Errorf("event instanceKey = %q, want %q", ev.instanceKey, key)
		}
		if ev.env.Type != control.TypeOpenBrowser {
			t.Errorf("event Type = %q, want %q", ev.env.Type, control.TypeOpenBrowser)
		}
		var p control.OpenBrowserPayload
		if err := json.Unmarshal(ev.env.Payload, &p); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if p.URL != wantURL {
			t.Errorf("payload URL = %q, want %q", p.URL, wantURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for control event on registry channel")
	}

	// 4. When the instance is no longer running, the next sync drops it.
	r.syncRunning(&state.State{Fleets: map[string]*fleet.Fleet{}})
	if _, ok := r.servers[key]; ok {
		t.Fatalf("registry still has server for stopped instance %q", key)
	}
}

// TestShutdownFullBuffer is the regression guard for the blocked-send
// deadlock: the per-instance handler's send to r.events must be non-blocking.
// If a client floods more browser.open messages than the events buffer holds
// while nothing drains the channel, the handler goroutines must drop the
// overflow rather than block inside control.Server.serveConn — otherwise the
// server's WaitGroup never unwinds and shutdown() (which calls Server.Close →
// wg.Wait) hangs forever. The test sends far more than the buffer's capacity
// with no drainer, then asserts shutdown() returns promptly. (The test name is
// kept short so the temp-HOME socket path stays under the unix sun_path limit.)
func TestShutdownFullBuffer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)

	r := newControlRegistry()

	r.syncRunning(fabricatedRunningState(fleetName, instanceName))
	socketPath := state.ControlSocketPath(fleetName, instanceName)
	if !waitForSocket(t, socketPath, 2*time.Second) {
		t.Fatalf("control socket %q never appeared after syncRunning", socketPath)
	}

	client, err := control.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial(%q): %v", socketPath, err)
	}
	defer client.Close()

	// Send well beyond the events buffer (16) with nothing draining it, so the
	// handler hits the full-buffer path repeatedly.
	for i := 0; i < 256; i++ {
		if err := client.OpenBrowser("http://localhost:3000"); err != nil {
			t.Fatalf("OpenBrowser #%d: %v", i, err)
		}
	}

	// shutdown must return promptly even though the buffer is full and undrained.
	done := make(chan struct{})
	go func() {
		r.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown() hung with a full, undrained events buffer (blocked-send deadlock)")
	}
}

// TestSyncRunningIdempotent verifies repeated syncs against an unchanged
// running set neither leak nor recreate listeners: the same server pointer is
// kept across calls.
func TestSyncRunningIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		fleetName    = "alpha"
		instanceName = "i1"
	)
	key := fleetName + "/" + instanceName

	r := newControlRegistry()
	defer r.shutdown()

	st := fabricatedRunningState(fleetName, instanceName)
	r.syncRunning(st)
	first := r.servers[key]
	if first == nil {
		t.Fatalf("no server after first sync")
	}

	r.syncRunning(st)
	if second := r.servers[key]; second != first {
		t.Fatalf("second sync replaced the server pointer (%p → %p)", first, second)
	}
	if len(r.servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(r.servers))
	}
}

// TestWaitForControlEventCmd verifies the waiter command returns the queued
// event wrapped as a controlEventMsg.
func TestWaitForControlEventCmd(t *testing.T) {
	ch := make(chan controlEvent, 1)
	want := controlEvent{
		instanceKey: "alpha/i1",
		env:         control.Envelope{Type: control.TypeOpenBrowser},
	}
	ch <- want

	msg := waitForControlEventCmd(ch)()

	got, ok := msg.(controlEventMsg)
	if !ok {
		t.Fatalf("msg type = %T, want controlEventMsg", msg)
	}
	if got.instanceKey != want.instanceKey {
		t.Errorf("instanceKey = %q, want %q", got.instanceKey, want.instanceKey)
	}
	if got.env.Type != want.env.Type {
		t.Errorf("env.Type = %q, want %q", got.env.Type, want.env.Type)
	}
}

// TestSplitInstanceKey covers the "<fleet>/<instance>" splitter, including a
// malformed key with no separator.
func TestSplitInstanceKey(t *testing.T) {
	cases := []struct {
		key          string
		wantFleet    string
		wantInstance string
		wantOK       bool
	}{
		{"alpha/i1", "alpha", "i1", true},
		{"a/b/c", "a", "b/c", true}, // splits on the first separator
		{"noseparator", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			f, i, ok := splitInstanceKey(tc.key)
			if f != tc.wantFleet || i != tc.wantInstance || ok != tc.wantOK {
				t.Errorf("splitInstanceKey(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.key, f, i, ok, tc.wantFleet, tc.wantInstance, tc.wantOK)
			}
		})
	}
}

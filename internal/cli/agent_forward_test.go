package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

func stubAgentForward(t *testing.T, enabled bool) (started *atomic.Int32, stopped *atomic.Int32) {
	t.Helper()
	// A developer running the tests from a TUI's tmux pane inherits its pid.
	t.Setenv(fleetclient.EnvAgentProviderPID, "")
	origEnabled, origRun := agentForwardEnabled, runAgentProvider
	t.Cleanup(func() { agentForwardEnabled, runAgentProvider = origEnabled, origRun })
	var asked atomic.Value
	agentForwardEnabled = func(_ context.Context, url string) bool {
		asked.Store(url)
		return enabled
	}
	started, stopped = new(atomic.Int32), new(atomic.Int32)
	runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, _ string, report func(agentfwd.Status)) {
		started.Add(1)
		report(agentfwd.Status{State: agentfwd.StateActive})
		<-ctx.Done()
		stopped.Add(1)
	}
	return started, stopped
}

// captureAgentNotice collects what forwardAgentWhile tells the user. Read it
// only after stop() returned (the provider goroutine writes it).
func captureAgentNotice(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := agentForwardNotice
	t.Cleanup(func() { agentForwardNotice = orig })
	agentForwardNotice = &buf
	return &buf
}

func TestForwardAgentWhileProvidesForTheCommandsDuration(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	started, stopped := stubAgentForward(t, true)

	stop := forwardAgentWhile(context.Background(), nil)
	if started.Load() != 1 {
		t.Fatal("the provider should be attached before the command's work starts")
	}
	if stopped.Load() != 0 {
		t.Fatal("the provider must run for the command's duration")
	}
	stop()
	if stopped.Load() != 1 {
		t.Fatal("stop should end the provider and wait for it")
	}
}

func TestForwardAgentWhileIsANoOpWhenOff(t *testing.T) {
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	started, _ := stubAgentForward(t, false)
	forwardAgentWhile(context.Background(), nil)()
	if started.Load() != 0 {
		t.Fatal("no provider when forwarding is off for the remote")
	}
}

func TestForwardAgentWhileDoesNotWaitForeverOnASilentProvider(t *testing.T) {
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	stubAgentForward(t, true)
	runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, _ string, report func(agentfwd.Status)) {
		report(agentfwd.Status{State: agentfwd.StateConnecting})
		<-ctx.Done()
	}
	orig := agentForwardAttachWait
	t.Cleanup(func() { agentForwardAttachWait = orig })
	agentForwardAttachWait = 200 * time.Millisecond
	start := time.Now()
	stop := forwardAgentWhile(context.Background(), nil)
	stop()
	if elapsed := time.Since(start); elapsed > agentForwardAttachWait+2*time.Second {
		t.Fatalf("waited %s for a provider that never attached", elapsed)
	}
}

// deadPID is the pid of a child that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// TestForwardAgentWhileDefersToALiveTUI: a shell the TUI spawned must not
// become a competing provider while that TUI runs; once the TUI has exited
// (its pid left behind in a tmux pane's env) the command provides as usual.
func TestForwardAgentWhileDefersToALiveTUI(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	started, _ := stubAgentForward(t, true)

	t.Setenv(fleetclient.EnvAgentProviderPID, strconv.Itoa(os.Getpid()))
	forwardAgentWhile(context.Background(), nil)()
	if started.Load() != 0 {
		t.Fatal("no provider while the TUI that spawned this command is alive")
	}

	t.Setenv(fleetclient.EnvAgentProviderPID, strconv.Itoa(deadPID(t)))
	forwardAgentWhile(context.Background(), nil)()
	if started.Load() != 1 {
		t.Fatal("the command should provide once the TUI has exited")
	}
}

func TestTUIProvidesAgent(t *testing.T) {
	for _, raw := range []string{"", "not-a-pid", "0", "-1"} {
		t.Setenv(fleetclient.EnvAgentProviderPID, raw)
		if tuiProvidesAgent() {
			t.Errorf("%s=%q: want no live provider", fleetclient.EnvAgentProviderPID, raw)
		}
	}
	t.Setenv(fleetclient.EnvAgentProviderPID, strconv.Itoa(os.Getpid()))
	if !tuiProvidesAgent() {
		t.Error("a live pid should count as a providing TUI")
	}
	t.Setenv(fleetclient.EnvAgentProviderPID, strconv.Itoa(deadPID(t)))
	if tuiProvidesAgent() {
		t.Error("a dead pid should not count as a providing TUI")
	}
	// Another user's live process answers EPERM: still alive.
	if err := syscall.Kill(1, 0); errors.Is(err, syscall.EPERM) {
		t.Setenv(fleetclient.EnvAgentProviderPID, "1")
		if !tuiProvidesAgent() {
			t.Error("EPERM means the process exists")
		}
	}
}

// TestForwardAgentWhileStopsWhenForwardingIsTurnedOff: the provider re-reads
// the registry while it runs and stops once forwarding is off; the command
// itself keeps going (stop is still the caller's to call).
func TestForwardAgentWhileStopsWhenForwardingIsTurnedOff(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	started, stopped := stubAgentForward(t, true)
	var enabled atomic.Bool
	enabled.Store(true)
	var checks atomic.Int32
	agentForwardEnabled = func(_ context.Context, url string) bool {
		if url != "ssh://ben@devbox" {
			t.Errorf("re-checked %q, want the command's remote", url)
		}
		checks.Add(1)
		return enabled.Load()
	}
	orig := agentForwardRecheck
	t.Cleanup(func() { agentForwardRecheck = orig })
	agentForwardRecheck = 10 * time.Millisecond

	stop := forwardAgentWhile(context.Background(), nil)
	defer stop()
	if started.Load() != 1 {
		t.Fatal("the provider should start while forwarding is on")
	}
	waitUntil(t, "a few re-checks", func() bool { return checks.Load() >= 4 })
	if stopped.Load() != 0 {
		t.Fatal("the provider must keep running while forwarding stays on")
	}

	enabled.Store(false)
	waitUntil(t, "the provider to stop", func() bool { return stopped.Load() == 1 })
	stop()
	if started.Load() != 1 || stopped.Load() != 1 {
		t.Fatalf("started %d, stopped %d: want one provider, stopped once", started.Load(), stopped.Load())
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestForwardAgentWhileTellsTheUserOnce: the first reason forwarding cannot
// work is printed as one line; repeats (the provider retries) and later
// reasons stay quiet, and a working provider prints nothing.
func TestForwardAgentWhileTellsTheUserOnce(t *testing.T) {
	cases := []struct {
		name    string
		reports []agentfwd.Status
		want    string
	}{
		{
			name: "no local agent",
			reports: []agentfwd.Status{
				{State: agentfwd.StateNoAgent, Detail: "SSH_AUTH_SOCK is not set"},
				{State: agentfwd.StateNoAgent, Detail: "SSH_AUTH_SOCK is not set"},
				{State: agentfwd.StateRefused, Detail: "turned off on this host"},
			},
			want: "fleet: SSH agent forwarding: SSH_AUTH_SOCK is not set\n",
		},
		{
			name: "refused by the remote",
			reports: []agentfwd.Status{
				{State: agentfwd.StateConnecting},
				{State: agentfwd.StateRefused, Detail: "turned off on this host"},
				{State: agentfwd.StateConnecting},
				{State: agentfwd.StateRefused, Detail: "turned off on this host"},
			},
			want: "fleet: SSH agent forwarding refused by the remote: turned off on this host\n",
		},
		{
			name:    "working",
			reports: []agentfwd.Status{{State: agentfwd.StateConnecting}, {State: agentfwd.StateActive, Uses: 1}},
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(fleetclient.EnvGateway, "")
			t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
			stubAgentForward(t, true)
			out := captureAgentNotice(t)
			runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, _ string, report func(agentfwd.Status)) {
				for _, st := range c.reports {
					report(st)
				}
				<-ctx.Done()
			}
			forwardAgentWhile(context.Background(), nil)()
			if got := out.String(); got != c.want {
				t.Fatalf("notice = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCurrentRemoteURL(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	if got := currentRemoteURL(); got != "" {
		t.Fatalf("local: %q", got)
	}
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	if got := currentRemoteURL(); got != "ssh://ben@devbox" {
		t.Fatalf("ssh: %q", got)
	}
	t.Setenv(fleetclient.EnvGateway, "https://gw.example.com/abc")
	if got := currentRemoteURL(); got != "https://gw.example.com/abc" {
		t.Fatalf("gateway wins like the endpoint selection: %q", got)
	}
}

// TestAgentForwardEnabledWithoutADaemonIsOff: with no local daemon listening
// the lookup answers off (and, through ProbeLocalArmada, never spawns one).
func TestAgentForwardEnabledWithoutADaemonIsOff(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "fmagent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	if agentForwardEnabled(context.Background(), "ssh://ben@devbox") {
		t.Fatal("no daemon, no registry: forwarding must be off")
	}
	if agentForwardEnabled(context.Background(), "") {
		t.Fatal("a local connection never forwards")
	}
}

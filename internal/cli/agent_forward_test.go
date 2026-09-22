package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

func stubAgentForward(t *testing.T, enabled bool) (started *atomic.Int32, stopped *atomic.Int32) {
	t.Helper()
	// A test run from a TUI-spawned pane inherits the TUI's hint; every test
	// starts without it and sets it explicitly when it wants it.
	t.Setenv(fleetclient.EnvTUIProvidesAgent, "")
	origState, origRun := agentForwardState, runAgentProvider
	t.Cleanup(func() { agentForwardState, runAgentProvider = origState, origRun })
	agentForwardState = func(context.Context, string) (bool, error) {
		return enabled, nil
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
	agentForwardState = func(_ context.Context, url string) (bool, error) {
		if url != "ssh://ben@devbox" {
			t.Errorf("re-checked %q, want the command's remote", url)
		}
		checks.Add(1)
		return enabled.Load(), nil
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

// TestForwardAgentWhileKeepsForwardingWhenTheRegistryIsUnreadable: a
// re-check that cannot read the registry (the local daemon restarting) keeps
// the provider running; only a registry that was read and says off stops it.
func TestForwardAgentWhileKeepsForwardingWhenTheRegistryIsUnreadable(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	started, stopped := stubAgentForward(t, true)
	// 0: on, 1: unreadable, 2: off.
	var phase, checks atomic.Int32
	agentForwardState = func(context.Context, string) (bool, error) {
		checks.Add(1)
		switch phase.Load() {
		case 0:
			return true, nil
		case 1:
			return false, errors.New("connection refused")
		default:
			return false, nil
		}
	}
	orig := agentForwardRecheck
	t.Cleanup(func() { agentForwardRecheck = orig })
	agentForwardRecheck = 10 * time.Millisecond

	stop := forwardAgentWhile(context.Background(), nil)
	defer stop()
	if started.Load() != 1 {
		t.Fatal("the provider should start while forwarding is on")
	}
	phase.Store(1)
	from := checks.Load()
	waitUntil(t, "re-checks against an unreadable registry", func() bool {
		return checks.Load() >= from+4 || stopped.Load() != 0
	})
	if stopped.Load() != 0 {
		t.Fatal("an unreadable registry must not stop the provider")
	}

	phase.Store(2)
	waitUntil(t, "the provider to stop", func() bool { return stopped.Load() == 1 })
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

// TestForwardAgentWhileIsSilentOnceItReturned: after forwardAgentWhile has
// returned the command owns the terminal (a remote shell in raw mode), so a
// later reason forwarding cannot work prints nothing.
func TestForwardAgentWhileIsSilentOnceItReturned(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	stubAgentForward(t, true)
	out := captureAgentNotice(t)
	late := make(chan agentfwd.Status)
	delivered := make(chan struct{})
	runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, _ string, report func(agentfwd.Status)) {
		report(agentfwd.Status{State: agentfwd.StateActive})
		for {
			select {
			case st := <-late:
				report(st)
				delivered <- struct{}{}
			case <-ctx.Done():
				return
			}
		}
	}

	stop := forwardAgentWhile(context.Background(), nil)
	for _, st := range []agentfwd.Status{
		{State: agentfwd.StateNoAgent, Detail: "SSH_AUTH_SOCK is not set"},
		{State: agentfwd.StateRefused, Detail: "turned off on this host"},
	} {
		late <- st
		<-delivered
	}
	stop()
	if got := out.String(); got != "" {
		t.Fatalf("notice after forwardAgentWhile returned = %q, want nothing", got)
	}
}

// TestForwardAgentWhileSpawnedByTheTUIDoesNotWait: a shell the TUI spawned
// (fleetclient.EnvTUIProvidesAgent) starts its provider but does not wait
// for it to attach — the TUI's provider already serves the remote — and it
// prints nothing even once the provider reports a reason it cannot work.
func TestForwardAgentWhileSpawnedByTheTUIDoesNotWait(t *testing.T) {
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	started, stopped := stubAgentForward(t, true)
	t.Setenv(fleetclient.EnvTUIProvidesAgent, "1")
	out := captureAgentNotice(t)
	orig := agentForwardAttachWait
	t.Cleanup(func() { agentForwardAttachWait = orig })
	agentForwardAttachWait = 10 * time.Second
	// The provider settles only once released: without the hint the wait
	// would run out its full attachWait.
	release := make(chan struct{})
	reported := make(chan struct{})
	runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, _ string, report func(agentfwd.Status)) {
		started.Add(1)
		report(agentfwd.Status{State: agentfwd.StateConnecting})
		<-release
		report(agentfwd.Status{State: agentfwd.StateNoAgent, Detail: "SSH_AUTH_SOCK is not set"})
		close(reported)
		<-ctx.Done()
		stopped.Add(1)
	}

	start := time.Now()
	stop := forwardAgentWhile(context.Background(), nil)
	if elapsed := time.Since(start); elapsed >= agentForwardAttachWait/2 {
		t.Errorf("waited %s for the provider although the TUI provides the agent", elapsed)
	}
	waitUntil(t, "the provider to start", func() bool { return started.Load() == 1 })
	close(release)
	<-reported
	if stopped.Load() != 0 {
		t.Fatal("the provider must run for the command's duration")
	}
	stop()
	if stopped.Load() != 1 {
		t.Fatal("stop should end the provider and wait for it")
	}
	if got := out.String(); got != "" {
		t.Fatalf("notice = %q, want nothing: the TUI shows the forwarding state", got)
	}
}

// TestForwardAgentWhileLeavesTheNoticeToTheTUI: a reason forwarding cannot
// work that the provider reports at once is printed by a command the user
// ran, and not by one the TUI spawned (which still starts its provider).
func TestForwardAgentWhileLeavesTheNoticeToTheTUI(t *testing.T) {
	for _, c := range []struct {
		hint string
		want string
	}{
		{hint: "", want: "fleet: SSH agent forwarding: SSH_AUTH_SOCK is not set\n"},
		{hint: "1", want: ""},
	} {
		t.Run("hint="+c.hint, func(t *testing.T) {
			t.Setenv(fleetclient.EnvGateway, "")
			t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
			started, _ := stubAgentForward(t, true)
			t.Setenv(fleetclient.EnvTUIProvidesAgent, c.hint)
			out := captureAgentNotice(t)
			reported := make(chan struct{})
			runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, _ string, report func(agentfwd.Status)) {
				started.Add(1)
				report(agentfwd.Status{State: agentfwd.StateNoAgent, Detail: "SSH_AUTH_SOCK is not set"})
				close(reported)
				<-ctx.Done()
			}
			stop := forwardAgentWhile(context.Background(), nil)
			<-reported
			stop()
			if started.Load() != 1 {
				t.Fatalf("providers started = %d, want 1", started.Load())
			}
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
// the lookup answers off (and, through ProbeLocalArmada, never spawns one),
// and the state read reports it as unreadable rather than as off — the
// distinction the running provider's re-check relies on.
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
	if _, err := agentForwardState(context.Background(), "ssh://ben@devbox"); err == nil {
		t.Fatal("no daemon: the registry could not be read, want an error")
	}
	if agentForwardEnabled(context.Background(), "") {
		t.Fatal("a local connection never forwards")
	}
	if enabled, err := agentForwardState(context.Background(), ""); enabled || err != nil {
		t.Fatalf("local connection: state = %v, %v; want off with no error", enabled, err)
	}
}

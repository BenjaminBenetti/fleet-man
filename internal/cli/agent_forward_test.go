package cli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

func stubAgentForward(t *testing.T, enabled bool) (started *atomic.Int32, stopped *atomic.Int32) {
	t.Helper()
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

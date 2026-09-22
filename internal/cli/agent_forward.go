package cli

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// agent_forward.go lets a CLI command carry the user's ssh-agent to a remote
// fleet the way an open TUI does: while `fleet up`/`clone`/`rebuild`/`shell`
// runs against a registered Armada remote with "Forward SSH agent" on, this
// process provides its agent over the SSHAgent stream, so the remote's clone
// and the shell's git use the user's keys. Best-effort throughout: a command
// never fails because forwarding could not start.

// agentForwardLookupTimeout bounds reading the registry from the local daemon.
const agentForwardLookupTimeout = 3 * time.Second

// agentForwardAttachWait bounds how long a command waits for its provider to
// attach before starting work, so a job's first clone already finds it. A var
// so tests need not wait it out.
var agentForwardAttachWait = 5 * time.Second

// currentRemoteURL is the Armada URL the command is connected through
// (FLEET_GATEWAY or FLEET_SSH), "" for local or a plain FLEET_SERVER target.
func currentRemoteURL() string {
	if gw := os.Getenv(fleetclient.EnvGateway); gw != "" {
		return gw
	}
	return os.Getenv(fleetclient.EnvSSH)
}

// agentForwardEnabled reports whether url is a registered Armada remote with
// forwarding on. It asks the LOCAL daemon (which holds the registry) only if
// one is already running: forwarding must never be the reason a gateway-only
// user suddenly gets a local daemon spawned. A var so tests can stub it.
var agentForwardEnabled = func(ctx context.Context, url string) bool {
	if url == "" {
		return false
	}
	if _, err := os.Stat(fleetpaths.SocketPath()); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, agentForwardLookupTimeout)
	defer cancel()
	local, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return false
	}
	defer local.Close()
	reply, err := local.Service().GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		return false
	}
	for _, r := range reply.GetRemotes() {
		if r.GetUrl() == url {
			return r.GetForwardAgent()
		}
	}
	return false
}

// runAgentProvider is agentfwd.Run; a var so tests need no daemon.
var runAgentProvider = agentfwd.Run

// forwardAgentWhile starts providing this machine's ssh-agent to svc when the
// current connection has forwarding on, waits briefly for it to attach, and
// returns the function that stops it (a no-op when nothing started).
func forwardAgentWhile(ctx context.Context, svc fleetgrpc.FleetServiceClient) (stop func()) {
	if !agentForwardEnabled(ctx, currentRemoteURL()) {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	// report runs on the provider's goroutines, possibly concurrently: the
	// first settled state (attached, or a reason it cannot) releases the wait.
	settled := make(chan struct{})
	var settle sync.Once
	report := func(st agentfwd.Status) {
		if st.State != agentfwd.StateConnecting {
			settle.Do(func() { close(settled) })
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentProvider(ctx, svc, "cli", report)
	}()
	select {
	case <-settled:
	case <-done:
	case <-time.After(agentForwardAttachWait):
	}
	return func() {
		cancel()
		<-done
	}
}

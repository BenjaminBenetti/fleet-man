package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// agent_forward.go lets a CLI command carry the user's ssh-agent to a remote
// fleet the way an open TUI does: while `fleet up`/`clone`/`rebuild`/`shell`
// runs against a registered Armada remote with [ agent: on ], this process
// provides its agent over the SSHAgent stream, so the remote's clone and the
// shell's git use the user's keys. Best-effort throughout: a command never
// fails because forwarding could not start.

// agentForwardLookupTimeout bounds reading the registry from the local daemon.
const agentForwardLookupTimeout = 3 * time.Second

// agentForwardAttachWait bounds how long a command waits for its provider to
// attach before starting work, so a job's first clone already finds it. A var
// so tests need not wait it out.
var agentForwardAttachWait = 5 * time.Second

// agentForwardRecheck is how often a running provider re-reads the registry,
// so turning [ agent: on ] off also reaches a long-lived `fleet shell` rather
// than only the commands started afterwards. A var so tests can shorten it.
var agentForwardRecheck = 30 * time.Second

// agentForwardNotice receives the one line a command prints when forwarding
// cannot work. A var so tests can capture it.
var agentForwardNotice io.Writer = os.Stderr

// currentRemoteURL is the Armada URL the command is connected through
// (FLEET_GATEWAY or FLEET_SSH), "" for local or a plain FLEET_SERVER target.
func currentRemoteURL() string {
	if gw := os.Getenv(fleetclient.EnvGateway); gw != "" {
		return gw
	}
	return os.Getenv(fleetclient.EnvSSH)
}

// agentForwardEnabled reports whether url is a registered Armada remote with
// forwarding on. It reads the registry from the LOCAL daemon only if one is
// already running, through a probe that never spawns or restarts one:
// forwarding must never be the reason a gateway-only user suddenly gets a
// local daemon, nor the reason a running one is relaunched under its other
// clients. A registry that cannot be read counts as off: the agent is only
// handed out while the registry says so. A var so tests can stub it.
var agentForwardEnabled = func(ctx context.Context, url string) bool {
	if url == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, agentForwardLookupTimeout)
	defer cancel()
	remotes, err := fleetclient.ProbeLocalArmada(ctx)
	if err != nil {
		return false
	}
	for _, r := range remotes {
		if r.GetUrl() == url {
			return r.GetForwardAgent()
		}
	}
	return false
}

// tuiProvidesAgent reports whether the TUI that spawned this command is still
// running: it set fleetclient.EnvAgentProviderPID to its pid, and it provides
// the agent itself under the same registry. EPERM counts as alive — the
// process exists, it just is not ours to signal. A dead pid (the TUI quit and
// left the variable behind in a tmux pane) means this command provides.
func tuiProvidesAgent() bool {
	pid, err := strconv.Atoi(os.Getenv(fleetclient.EnvAgentProviderPID))
	// kill(2) reads pid 0 and negative pids as process groups: never probe them.
	if err != nil || pid <= 0 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// runAgentProvider is agentfwd.Run; a var so tests need no daemon.
var runAgentProvider = agentfwd.Run

// forwardAgentWhile starts providing this machine's ssh-agent to svc when the
// current connection has forwarding on, waits briefly for it to attach, and
// returns the function that stops it (a no-op when nothing started). While it
// runs it follows the registry: turning forwarding off stops the provider,
// and the command carries on without it.
func forwardAgentWhile(ctx context.Context, svc fleetgrpc.FleetServiceClient) (stop func()) {
	if tuiProvidesAgent() {
		// A second provider would only become the newest one and push the
		// TUI's to standby while this command runs.
		return func() {}
	}
	url := currentRemoteURL()
	if !agentForwardEnabled(ctx, url) {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	// report runs on the provider's goroutines: the first settled state
	// (attached, or a reason it cannot) releases the wait, and the first
	// reason it cannot work is worth one line — silently doing nothing would
	// leave the user wondering why their keys are not used.
	settled := make(chan struct{})
	var settle, notice sync.Once
	report := func(st agentfwd.Status) {
		switch st.State {
		case agentfwd.StateConnecting:
			return
		case agentfwd.StateRefused:
			notice.Do(func() {
				fmt.Fprintf(agentForwardNotice, "fleet: SSH agent forwarding refused by the remote: %s\n", st.Detail)
			})
		case agentfwd.StateNoAgent:
			notice.Do(func() {
				fmt.Fprintf(agentForwardNotice, "fleet: SSH agent forwarding: %s\n", st.Detail)
			})
		}
		settle.Do(func() { close(settled) })
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentProvider(ctx, svc, "cli", report)
	}()
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		ticker := time.NewTicker(agentForwardRecheck)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if !agentForwardEnabled(ctx, url) {
					cancel()
					return
				}
			}
		}
	}()
	select {
	case <-settled:
	case <-done:
	case <-time.After(agentForwardAttachWait):
	}
	return func() {
		cancel()
		<-done
		<-followed
	}
}

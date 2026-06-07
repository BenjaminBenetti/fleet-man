package tui

import (
	"context"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
)

// tuiConnectedOnce ensures the server's FleetTUIConnected hook fires exactly
// once per TUI process — on the first successful Watch open — rather than on
// every reconnect. "Once per TUI opening" is the contract; the server coalesces
// across multiple TUIs anyway.
var tuiConnectedOnce sync.Once

// Watch-stream messages injected into the bubbletea loop by the watcher
// goroutine. In P2 Step 5 these only populate the m.pstate / m.runtime caches;
// the View still renders from the legacy fields until Step 6/7 flip the read
// path. BrowserOpen is a no-op stub (control stays TUI-side in P2; see the P2
// plan's Decision 2).
type stateChangedMsg struct{ state *fleetgrpc.State }
type runtimeChangedMsg struct{ runtime []*fleetgrpc.InstanceRuntime }
type watchBrowserOpenMsg struct {
	url, dataDir, fleet, instance string
}
type remoteMcpStatusMsg struct{ status *fleetgrpc.RemoteMcpStatus }
type watchErrMsg struct{ err error }
type watchClosedMsg struct{ err error }

const (
	watchReconnectInitial = 250 * time.Millisecond
	watchReconnectMax     = 5 * time.Second
)

// runWatchStream maintains a single Watch subscription to the fleet server for
// the TUI's lifetime, injecting decoded events into the bubbletea program via
// program.Send. It reconnects with backoff if the stream drops — e.g. when the
// version handshake replaces a stale dev server, or the server restarts. The
// caller cancels ctx after program.Run() returns.
func runWatchStream(ctx context.Context, program *tea.Program) {
	backoff := watchReconnectInitial
	for {
		if ctx.Err() != nil {
			return
		}
		streamed := watchOnce(ctx, program)
		if streamed {
			// The stream opened and later ended (server shutdown/restart);
			// reconnect promptly with a small reset backoff.
			backoff = watchReconnectInitial
		} else {
			// Connect-level failure; back off before retrying.
			backoff = min(backoff*2, watchReconnectMax)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// watchOnce opens one connection + Watch stream and pumps events until it ends.
// It returns true if the stream opened (so the caller treats the end as a normal
// drop), false on a connect-level failure (Dial / Watch).
func watchOnce(ctx context.Context, program *tea.Program) bool {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		program.Send(watchErrMsg{err: err})
		return false
	}
	defer conn.Close()

	stream, err := conn.Service().Watch(ctx, &fleetgrpc.WatchRequest{
		IncludeInitialState: true,
		SubscribeRuntime:    true,
	}, grpc.WaitForReady(true))
	if err != nil {
		program.Send(watchErrMsg{err: err})
		return false
	}

	// A successful Watch open means the TUI has connected to a live server.
	// Nudge the server's once-per-open reconciliation, on a separate goroutine
	// so the bounded RPC never stalls this event pump, and only once per launch
	// (not on reconnects). The error is intentionally dropped: this is a pure
	// best-effort nudge, the SERVER logs the reconcile's own outcome, and a
	// failed nudge means the server is unreachable — already surfaced to the
	// user via watchErrMsg. (Client code also must not write the server-owned
	// event log, per the import boundary.)
	tuiConnectedOnce.Do(func() { go func() { _ = notifyTUIConnectedRemote() }() })

	for {
		ev, err := stream.Recv()
		if err != nil {
			program.Send(watchClosedMsg{err: err})
			return true
		}
		switch k := ev.GetKind().(type) {
		case *fleetgrpc.Event_StateChanged:
			program.Send(stateChangedMsg{state: k.StateChanged.GetState()})
		case *fleetgrpc.Event_RuntimeChanged:
			program.Send(runtimeChangedMsg{runtime: k.RuntimeChanged.GetRuntime()})
		case *fleetgrpc.Event_BrowserOpen:
			bo := k.BrowserOpen
			program.Send(watchBrowserOpenMsg{
				url:      bo.GetUrl(),
				dataDir:  bo.GetDataDir(),
				fleet:    bo.GetFleet(),
				instance: bo.GetInstance(),
			})
		case *fleetgrpc.Event_RemoteMcpStatus:
			program.Send(remoteMcpStatusMsg{status: k.RemoteMcpStatus})
		default:
			// Job* events are not consumed by the TUI in P2.
		}
	}
}

// rtKey is the m.runtime map key joining a runtime sidecar to its persisted
// instance (matches the server's runtimeKey).
func rtKey(fleetName, instance string) string { return fleetName + "/" + instance }

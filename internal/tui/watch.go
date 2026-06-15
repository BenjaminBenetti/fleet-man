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

// watchCtl coordinates the watch-stream goroutine's lifetime so the armada
// switch can bounce it onto a new endpoint: cancel the current stream's ctx,
// then start a fresh goroutine whose Dial re-reads the (swapped) env vars. It
// also owns the once-per-CONNECTION FleetTUIConnected nudge — fired on the
// first successful Watch open after each start/bounce, not on reconnects, so
// each daemon the TUI lands on gets its reconcile nudge exactly once.
//
// gen is the connection generation, bumped on every bounce. Each watch
// goroutine captures its gen at spawn and stamps it onto every message it
// sends; the model drops any message whose gen != m.watchGen so a stale event
// from a superseded (old-endpoint) stream can't repopulate the just-switched
// caches. notifiedConnected is also keyed to gen so a cancelled old goroutine
// can't consume the NEW connection's once-flag.
var watchCtl struct {
	mu          sync.Mutex
	parent      context.Context
	program     *tea.Program
	cancel      context.CancelFunc
	gen         int
	notifiedGen int // the gen for which the connect nudge has fired (-1 = none)
}

// startWatchStream starts the watch goroutine for the TUI's lifetime.
// Cancelling parent (Run() does, after program.Run returns) stops it and any
// bounced successor. Returns the initial generation (0).
func startWatchStream(parent context.Context, program *tea.Program) int {
	watchCtl.mu.Lock()
	defer watchCtl.mu.Unlock()
	watchCtl.parent = parent
	watchCtl.program = program
	watchCtl.gen = 0
	watchCtl.notifiedGen = -1
	ctx, cancel := context.WithCancel(parent)
	watchCtl.cancel = cancel
	go runWatchStream(ctx, program, watchCtl.gen)
	return watchCtl.gen
}

// bounceWatchStream kills the current watch stream and starts a fresh one,
// which dials whatever endpoint the env vars now select, and returns the new
// generation so the caller can stamp it onto the model. No-op (returns the
// current gen) when the stream was never started (tests).
func bounceWatchStream() int {
	watchCtl.mu.Lock()
	defer watchCtl.mu.Unlock()
	if watchCtl.cancel == nil {
		return watchCtl.gen
	}
	watchCtl.cancel()
	watchCtl.gen++
	ctx, cancel := context.WithCancel(watchCtl.parent)
	watchCtl.cancel = cancel
	go runWatchStream(ctx, watchCtl.program, watchCtl.gen)
	return watchCtl.gen
}

// notifyTUIConnectedOnce fires the server's FleetTUIConnected hook on the
// first successful Watch open of the given generation (see watchCtl). Keyed to
// gen so a reconnect within a connection doesn't re-fire it and a stale
// goroutine can't claim a newer connection's slot.
func notifyTUIConnectedOnce(gen int) {
	watchCtl.mu.Lock()
	already := watchCtl.notifiedGen == gen
	if !already {
		watchCtl.notifiedGen = gen
	}
	watchCtl.mu.Unlock()
	if !already {
		go func() { _ = notifyTUIConnectedRemote() }()
	}
}

// Watch-stream messages injected into the bubbletea loop by the watcher
// goroutine. Each carries the connection generation (gen) it was produced on;
// the model drops messages whose gen no longer matches the active connection
// so a stale event from a bounced stream can't repaint switched-away state.
// BrowserOpen is a no-op stub (control stays TUI-side in P2; see the P2 plan's
// Decision 2).
type stateChangedMsg struct {
	state *fleetgrpc.State
	gen   int
}
type runtimeChangedMsg struct {
	runtime []*fleetgrpc.InstanceRuntime
	gen     int
}
type watchBrowserOpenMsg struct {
	url, dataDir, fleet, instance string
	gen                           int
}
type watchFileCopyMsg struct {
	fleet, instance, src, dst string
	gen                       int
}
type remoteMcpStatusMsg struct {
	status *fleetgrpc.RemoteMcpStatus
	gen    int
}

// serverInfoMsg carries the daemon version learned at Dial time (Hello
// handshake), sent on every successful (re)connect so the header can render the
// control-chain versions.
type serverInfoMsg struct {
	serverVersion string
	gen           int
}
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
// caller cancels ctx after program.Run() returns. gen is this stream's
// connection generation, stamped onto every message so the model can drop
// events from a superseded stream after an armada switch.
func runWatchStream(ctx context.Context, program *tea.Program, gen int) {
	backoff := watchReconnectInitial
	for {
		if ctx.Err() != nil {
			return
		}
		streamed := watchOnce(ctx, program, gen)
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
func watchOnce(ctx context.Context, program *tea.Program, gen int) bool {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		program.Send(watchErrMsg{err: err})
		return false
	}
	defer conn.Close()

	// Surface the daemon's version (from Dial's Hello handshake) so the header
	// can render the control-chain versions. Reached through the gateway when
	// remote, so this is always fleetd's version.
	program.Send(serverInfoMsg{serverVersion: conn.ServerVersion(), gen: gen})

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
	// so the bounded RPC never stalls this event pump, and only once per
	// connection (not on reconnects; re-armed by an armada switch). The error
	// is intentionally dropped: this is a pure best-effort nudge, the SERVER
	// logs the reconcile's own outcome, and a failed nudge means the server is
	// unreachable — already surfaced to the user via watchErrMsg. (Client code
	// also must not write the server-owned event log, per the import boundary.)
	notifyTUIConnectedOnce(gen)

	for {
		ev, err := stream.Recv()
		if err != nil {
			program.Send(watchClosedMsg{err: err})
			return true
		}
		// Stop pumping the moment this stream is superseded: a bounce cancels
		// ctx, but a frame already buffered in Recv would otherwise still be
		// sent. Dropping it here (in addition to the model's gen check) keeps
		// stale frames off the bubbletea queue entirely.
		if ctx.Err() != nil {
			return true
		}
		switch k := ev.GetKind().(type) {
		case *fleetgrpc.Event_StateChanged:
			program.Send(stateChangedMsg{state: k.StateChanged.GetState(), gen: gen})
		case *fleetgrpc.Event_RuntimeChanged:
			program.Send(runtimeChangedMsg{runtime: k.RuntimeChanged.GetRuntime(), gen: gen})
		case *fleetgrpc.Event_BrowserOpen:
			bo := k.BrowserOpen
			program.Send(watchBrowserOpenMsg{
				url:      bo.GetUrl(),
				dataDir:  bo.GetDataDir(),
				fleet:    bo.GetFleet(),
				instance: bo.GetInstance(),
				gen:      gen,
			})
		case *fleetgrpc.Event_FileCopy:
			fc := k.FileCopy
			program.Send(watchFileCopyMsg{
				fleet:    fc.GetFleet(),
				instance: fc.GetInstance(),
				src:      fc.GetSrc(),
				dst:      fc.GetDst(),
				gen:      gen,
			})
		case *fleetgrpc.Event_RemoteMcpStatus:
			program.Send(remoteMcpStatusMsg{status: k.RemoteMcpStatus, gen: gen})
		default:
			// Job* events are not consumed by the TUI in P2.
		}
	}
}

// rtKey is the m.runtime map key joining a runtime sidecar to its persisted
// instance (matches the server's runtimeKey).
func rtKey(fleetName, instance string) string { return fleetName + "/" + instance }

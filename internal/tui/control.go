package tui

import (
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Control Registry
// ===========================================

// controlEvent carries a single Envelope received on an instance's control
// socket along with the key of the instance it came from. The host TUI keys
// listeners (and thus events) by "<fleet>/<instance>" so it can resolve the
// originating instance when it dispatches the message — e.g. to find the
// backend and browser data dir for a browser.open request.
type controlEvent struct {
	// instanceKey is "<fleet>/<instance>" — the same form fleet uses for
	// browser proxy bookkeeping (see fleetName + "/" + instance.Name).
	instanceKey string
	// env is the decoded control Envelope; the consumer switches on env.Type.
	env control.Envelope
}

// controlRegistry owns one control.Server per running instance and funnels
// every received Envelope into a single channel the Bubble Tea loop drains.
//
// The host can only receive control messages from instances it is currently
// listening for, so the registry's job is to keep its set of listeners in
// lock-step with the set of running instances. reload() drives that via
// syncRunning on every state refresh; shutdown() tears every listener down
// when the TUI exits. All three operations are concurrency-safe because the
// registry is shared between the bubbletea model (passed by value) and the
// per-server handler goroutines.
type controlRegistry struct {
	// mu guards servers so syncRunning and shutdown can run while handler
	// goroutines are live without racing on the map.
	mu sync.Mutex
	// servers maps instanceKey → its running listener. An entry exists exactly
	// while a listener is open for that instance.
	servers map[string]*control.Server
	// events is the single sink every per-instance handler writes to. It is
	// buffered so a burst of messages (or a slow bubbletea tick) doesn't block
	// the handler goroutine inside control.Server.
	events chan controlEvent
}

// newControlRegistry creates an empty registry with a buffered event channel.
// The buffer (16) absorbs short bursts of control messages without blocking
// the server's handler goroutines while the bubbletea loop catches up.
func newControlRegistry() *controlRegistry {
	return &controlRegistry{
		servers: make(map[string]*control.Server),
		events:  make(chan controlEvent, 16),
	}
}

// syncRunning reconciles the registry's listeners against the set of running
// instances in st. It starts a listener for every running instance that lacks
// one and Closes (and drops) listeners for instances that are gone or no
// longer running. It is idempotent and safe to call on every reload — the
// common case (no change to the running set) does nothing.
//
// Starting a listener is best-effort: a Listen failure (e.g. the per-instance
// control directory could not be created) is swallowed so a single bad
// instance can't break the registry or the reload that called it. The instance
// simply won't have host-side control until the next sync succeeds.
func (r *controlRegistry) syncRunning(st *state.State) {
	if st == nil {
		return
	}

	// Snapshot the desired set: every currently-running instance, keyed the
	// same way the registry keys its servers.
	want := make(map[string]struct{})
	for fleetName, f := range st.Fleets {
		for _, instance := range f.Instances {
			if instance.Status == fleet.StatusRunning {
				want[fleetName+"/"+instance.Name] = struct{}{}
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Drop listeners for instances that are gone or no longer running.
	for key, srv := range r.servers {
		if _, ok := want[key]; !ok {
			srv.Close()
			delete(r.servers, key)
		}
	}

	// Start listeners for running instances that lack one.
	for key := range want {
		if _, ok := r.servers[key]; ok {
			continue
		}
		fleetName, instanceName, ok := splitInstanceKey(key)
		if !ok {
			continue
		}
		socketPath := state.ControlSocketPath(fleetName, instanceName)
		// Capture key for the handler closure so every Envelope is tagged with
		// the instance it arrived from. The handler may run on multiple
		// goroutines; sending to a buffered channel is safe for concurrent use.
		//
		// The send is NON-BLOCKING (select+default) and that matters for more
		// than burst tolerance: the handler runs synchronously inside
		// control.Server.serveConn, whose goroutine is tracked by the server's
		// WaitGroup, and control.Server.Close waits on that WaitGroup. A blocking
		// send on a full buffer would wedge serveConn, so any Close of this server
		// would hang forever on wg.Wait(). That fires at shutdown (the drained-no-
		// longer Run loop has stopped reading r.events) and, worse, mid-session:
		// reload → syncRunning → srv.Close runs on the bubbletea Update goroutine,
		// which is the very goroutine that drains r.events, so a blocked handler
		// would deadlock the whole UI. Dropping on a full buffer (acceptable for
		// browser.open bursts) keeps serveConn — and therefore Close — unblockable.
		srv, err := control.Listen(socketPath, func(env control.Envelope) {
			select {
			case r.events <- controlEvent{instanceKey: key, env: env}:
			default:
				// Buffer full: drop rather than block the serveConn goroutine
				// (and any Close waiting on the server's WaitGroup).
			}
		})
		if err != nil {
			// Best-effort: a listen failure leaves this instance without
			// host-side control until the next sync. Don't fail the reload.
			continue
		}
		r.servers[key] = srv
	}
}

// shutdown closes every listener the registry owns and clears the set. Called
// once when the TUI exits so the host releases the socket files it created.
func (r *controlRegistry) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, srv := range r.servers {
		srv.Close()
		delete(r.servers, key)
	}
}

// splitInstanceKey splits a "<fleet>/<instance>" key back into its two parts.
// It returns ok=false for a malformed key (no separator) so callers can skip
// it rather than listen on a path derived from garbage.
func splitInstanceKey(key string) (fleetName, instanceName string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

// ===========================================
// Messages
// ===========================================

// controlEventMsg delivers a controlEvent into the bubbletea Update loop. The
// model-level Update switches on the wrapped env.Type to dispatch the message
// (e.g. TypeOpenBrowser → openControlBrowserCmd) and then re-arms the waiter.
type controlEventMsg controlEvent

// waitForControlEventCmd returns a tea.Cmd that blocks on the registry's event
// channel and delivers the next controlEvent as a controlEventMsg. The loop
// re-issues this command after handling each event, so a single in-flight
// waiter continuously drains the channel for the life of the program.
func waitForControlEventCmd(ch <-chan controlEvent) tea.Cmd {
	return func() tea.Msg {
		return controlEventMsg(<-ch)
	}
}

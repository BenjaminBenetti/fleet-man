package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// control.go owns the host-side control-socket listeners. An in-container
// process (e.g. a `fleet launch` TUI) writes browser.open envelopes to its
// instance's control socket; the SERVER listens on every running instance's
// socket and, for each envelope, invokes onOpen — which the server wires to the
// hub's BrowserOpen broadcast so the connected TUI execs its local browser.
//
// This moves the listeners off the thin client: the server owns ~/.fleet and
// all socket files, and the remote-TUI goal needs the URL resolved server-side
// with the client only launching the browser (see watch.proto BrowserOpen).

// controlSyncInterval is how often the registry reconciles its listener set
// against the running instances. The control socket only matters for running
// instances, and 1s matches the state poller's cadence.
const controlSyncInterval = time.Second

// controlRegistry owns one control.Server per running instance and forwards
// every received Envelope to the matching callback. syncRunning keeps the
// listener set in lock-step with the running instances; shutdown tears them all
// down. All operations are concurrency-safe (the registry is shared between the
// sync loop and the per-server handler goroutines).
type controlRegistry struct {
	// onOpen is invoked for each decoded browser.open envelope, tagged with the
	// originating instance. It MUST be safe for concurrent use (handlers run on
	// multiple goroutines) and non-blocking (it runs inside control.Server's
	// serveConn, whose goroutine Close waits on).
	onOpen func(fleetName, instanceName, url string)

	// onCopy is invoked for each decoded file.copy envelope, tagged with the
	// originating instance. src/dst are the scp endpoints as typed inside the
	// instance, passed through verbatim (the TUI runs the copy); open is the
	// `fleet open` flag (open the delivered file on the TUI machine). Same
	// concurrency/non-blocking contract as onOpen.
	onCopy func(fleetName, instanceName, src, dst string, open bool)

	// gated reports whether to keep control sockets open at all: only while a
	// browser-capable client (a TUI / Watch subscriber) is attached. With no
	// consumer, the sockets are torn down so an in-container `fleet launch`
	// reports "not connected to host fleet" instead of succeeding into the void.
	gated func() bool

	mu      sync.Mutex
	servers map[string]*control.Server // key: "<fleet>/<instance>"
}

// newControlRegistry creates an empty registry. onOpen is invoked per received
// browser.open envelope, onCopy per file.copy envelope; gated decides whether
// any sockets should be open (nil means always on).
func newControlRegistry(onOpen func(fleetName, instanceName, url string), onCopy func(fleetName, instanceName, src, dst string, open bool), gated func() bool) *controlRegistry {
	return &controlRegistry{
		onOpen:  onOpen,
		onCopy:  onCopy,
		gated:   gated,
		servers: make(map[string]*control.Server),
	}
}

// run reconciles listeners on a ticker until ctx is cancelled, then tears them
// all down. While no client is attached (gated()==false) it keeps every socket
// closed. Listener start is best-effort (a Listen failure leaves that instance
// without host-side control until the next sync).
func (r *controlRegistry) run(ctx context.Context) {
	defer r.shutdown()
	r.tick()
	ticker := time.NewTicker(controlSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

// tick syncs listeners against the running instances when a client is attached,
// or tears them all down when none is.
func (r *controlRegistry) tick() {
	if r.gated != nil && !r.gated() {
		r.shutdown()
		return
	}
	r.syncOnce()
}

// syncOnce loads state and reconciles the listener set. A load error keeps the
// current listeners and retries next tick.
func (r *controlRegistry) syncOnce() {
	st, err := state.Load()
	if err != nil {
		return
	}
	r.syncRunning(st)
}

// syncRunning starts a listener for every running instance lacking one and
// closes listeners for instances that are gone or no longer running. Idempotent.
func (r *controlRegistry) syncRunning(st *state.State) {
	if st == nil {
		return
	}
	want := make(map[string]struct{})
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.Status == fleet.StatusRunning {
				want[fleetName+"/"+inst.Name] = struct{}{}
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for key, srv := range r.servers {
		if _, ok := want[key]; !ok {
			srv.Close()
			delete(r.servers, key)
		}
	}

	for key := range want {
		if _, ok := r.servers[key]; ok {
			continue
		}
		fleetName, instanceName, ok := splitInstanceKey(key)
		if !ok {
			continue
		}
		socketPath := state.ControlSocketPath(fleetName, instanceName)
		// Capture key parts for the handler closure. The handler runs inside
		// control.Server.serveConn (whose goroutine Close waits on), so it MUST
		// NOT block — onOpen / the hub broadcast are non-blocking.
		fName, iName := fleetName, instanceName
		srv, err := control.Listen(socketPath, func(env control.Envelope) {
			switch env.Type {
			case control.TypeOpenBrowser:
				var payload control.OpenBrowserPayload
				if json.Unmarshal(env.Payload, &payload) != nil || payload.URL == "" {
					return
				}
				if r.onOpen != nil {
					flog.Info("browser open requested", "fleet", fName, "instance", iName, "url", payload.URL)
					r.onOpen(fName, iName, payload.URL)
				}
			case control.TypeCopyFile:
				var payload control.CopyFilePayload
				if json.Unmarshal(env.Payload, &payload) != nil || payload.Src == "" {
					return
				}
				if r.onCopy != nil {
					flog.Info("file copy requested", "fleet", fName, "instance", iName, "from", payload.Src, "to", payload.Dst, "open", payload.Open)
					r.onCopy(fName, iName, payload.Src, payload.Dst, payload.Open)
				}
			}
		})
		if err != nil {
			continue
		}
		r.servers[key] = srv
	}
}

// shutdown closes every listener and clears the set.
func (r *controlRegistry) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, srv := range r.servers {
		srv.Close()
		delete(r.servers, key)
	}
}

// splitInstanceKey splits a "<fleet>/<instance>" key on the first separator.
func splitInstanceKey(key string) (fleetName, instanceName string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

package server

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// statePollInterval is how often the server re-Loads state.json to detect
// changes. In P2 the server is NON-AUTHORITATIVE: legacy commands and the TUI
// still write state.json directly, so the hub's view of persisted state comes
// from polling the file. 1s is well within every integration-test tolerance,
// and proto.Equal-diffing means an unchanged file produces no broadcast.
//
// (fsnotify would be tighter but isn't a dependency yet; a ticker is
// dependency-free. The authoritative in-memory model replaces this in P4.)
const statePollInterval = time.Second

// runStatePoller is the hub's non-authoritative source of truth for persisted
// state: it re-Loads state.json on a ticker, converts to proto, and posts a
// setState closure. A direct state.Save by any writer therefore propagates to
// Watch subscribers within ~one interval.
func runStatePoller(ctx context.Context, h *hub) {
	loadAndPush(h) // immediate first load so a fresh subscriber isn't empty
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			loadAndPush(h)
		}
	}
}

func loadAndPush(h *hub) {
	st, err := state.Load()
	if err != nil {
		// A transient read error (e.g. a torn write mid-save) just means we keep
		// the previous snapshot and retry next tick — never push a partial state.
		return
	}
	snapshot := protoconv.StateToProto(st)
	h.post(func(h *hub) { h.setState(snapshot) })
}

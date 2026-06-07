package remote

import (
	"encoding/json"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// session.go persists the sticky gateway session so that, across reconnects and
// daemon restarts, fleetd asks the gateway for the SAME public URL. The file
// (~/.fleet/gateway_session.json, 0600) records the assigned session id, the
// public URL, and the gateway URL it was issued for.

type sessionFile struct {
	SessionID  string `json:"session_id"`
	PublicURL  string `json:"public_url"`
	GatewayURL string `json:"gateway_url"`
}

// loadSession returns the persisted session IF it was issued for gatewayURL. A
// stored session for a DIFFERENT gateway is stale (the id is meaningless to a
// new gateway), so it is ignored and a fresh registration is requested. A
// missing/unreadable/corrupt file yields a zero sessionFile (request a fresh id).
func loadSession(gatewayURL string) sessionFile {
	data, err := os.ReadFile(fleetpaths.GatewaySessionPath())
	if err != nil {
		return sessionFile{}
	}
	var s sessionFile
	if err := json.Unmarshal(data, &s); err != nil {
		return sessionFile{}
	}
	if s.GatewayURL != gatewayURL {
		return sessionFile{}
	}
	return s
}

// saveSession persists s atomically-enough for a best-effort hint (0600 — it is
// host-local and per-user, mirroring the other ~/.fleet discovery files). A
// write failure is returned but is non-fatal to the tunnel: a lost file just
// means the next reconnect gets a fresh URL.
func saveSession(s sessionFile) error {
	// ~/.fleet always exists by the time the daemon runs the manager (Serve
	// MkdirAlls it first), but ensure it so the write can't fail on a fresh dir.
	if _, err := fleetpaths.EnsureDir(); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(fleetpaths.GatewaySessionPath(), data, 0o600)
}

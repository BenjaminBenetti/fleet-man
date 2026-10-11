package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
)

// EditorEndpoints are the remote daemons an editor alias leads to that the
// armada does not hold: `fleet code` and the TUI's `c` record the remote they
// reached through FLEET_GATEWAY, FLEET_SSH or FLEET_SERVER, so `fleet
// ssh-proxy` (which OpenSSH starts without those variables) finds it again by
// the alias's remote hash. Kept beside armada.json with the same posture: its
// own file, written only by the LOCAL daemon, 0600 because a record can hold a
// bearer token.
type EditorEndpoints struct {
	// Endpoints by the alias's remote part ("r" + six hex digits).
	Endpoints map[string]EditorEndpoint `json:"endpoints,omitempty"`
}

// EditorEndpointKind is how an EditorEndpoint's address is reached.
type EditorEndpointKind string

const (
	// EditorEndpointGateway: Address is a FLEET_GATEWAY URL.
	EditorEndpointGateway EditorEndpointKind = "gateway"
	// EditorEndpointSSH: Address is a FLEET_SSH ssh:// URL.
	EditorEndpointSSH EditorEndpointKind = "ssh"
	// EditorEndpointServer: Address is a FLEET_SERVER host:port.
	EditorEndpointServer EditorEndpointKind = "server"
)

// Valid reports whether k is one of the kinds.
func (k EditorEndpointKind) Valid() bool {
	switch k {
	case EditorEndpointGateway, EditorEndpointSSH, EditorEndpointServer:
		return true
	}
	return false
}

// EditorEndpoint is one remote daemon.
type EditorEndpoint struct {
	Kind    EditorEndpointKind `json:"kind"`
	Address string             `json:"address"`
	// Token is the bearer token, only when it came from FLEET_TOKEN.
	Token string `json:"token,omitempty"`
}

// EditorEndpointsPath is the record file's path.
func EditorEndpointsPath() string {
	return filepath.Join(FleetDir(), "editor_endpoints.json")
}

// LoadEditorEndpoints reads the records; a missing file is an empty set.
func LoadEditorEndpoints() (*EditorEndpoints, error) {
	e := &EditorEndpoints{}
	data, err := os.ReadFile(EditorEndpointsPath())
	if os.IsNotExist(err) {
		return e, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading editor endpoints: %w", err)
	}
	if err := json.Unmarshal(data, e); err != nil {
		return nil, fmt.Errorf("parsing editor endpoints: %w", err)
	}
	e.applyDefaults()
	return e, nil
}

// SaveEditorEndpoints writes the records atomically (temp file + rename),
// 0600: a record can carry a bearer token.
func SaveEditorEndpoints(e *EditorEndpoints) error {
	if e == nil {
		e = &EditorEndpoints{}
	}
	e.applyDefaults()
	if err := os.MkdirAll(FleetDir(), 0755); err != nil {
		return fmt.Errorf("creating fleet dir: %w", err)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling editor endpoints: %w", err)
	}
	if err := atomicfile.Write(EditorEndpointsPath(), data, 0600); err != nil {
		return fmt.Errorf("writing editor endpoints: %w", err)
	}
	return nil
}

// applyDefaults trims every record and drops those with no address or an
// unknown kind.
func (e *EditorEndpoints) applyDefaults() {
	for key, ep := range e.Endpoints {
		ep.Address = strings.TrimSpace(ep.Address)
		ep.Token = strings.TrimSpace(ep.Token)
		if ep.Address == "" || !ep.Kind.Valid() {
			delete(e.Endpoints, key)
			continue
		}
		e.Endpoints[key] = ep
	}
}

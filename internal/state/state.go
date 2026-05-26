package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

var mu sync.Mutex

// State holds all fleet data.
type State struct {
	Fleets       map[string]*fleet.Fleet `json:"fleets"`
	GroupLayouts map[string]GroupLayout  `json:"groupLayouts,omitempty"`
	// LastSeenVersion is the fleet version whose release notes the user
	// has already been shown. When it differs from the compiled-in
	// version (e.g. after an upgrade) the TUI surfaces that version's
	// GitHub release notes once, then records the version here so the
	// dialog isn't shown again.
	LastSeenVersion string `json:"lastSeenVersion,omitempty"`
}

// FleetDir returns the base directory for fleet state.
func FleetDir() string {
	return filepath.Join(os.Getenv("HOME"), ".fleet")
}

// StatePath returns the path to the state file.
func StatePath() string {
	return filepath.Join(FleetDir(), "state.json")
}

// WorkspacesDir returns the base directory for instance workspace clones.
func WorkspacesDir() string {
	return filepath.Join(FleetDir(), "workspaces")
}

// WarnPath returns the path to the host-side warning file for a single
// instance. The TUI watches this path after StatusRunning and surfaces
// the first line as a banner — a non-existent file simply means
// "no warnings". Producers should use WriteWarn rather than constructing
// the path manually so all warnings end up in the same well-known place.
func WarnPath(fleetName, instanceName string) string {
	return filepath.Join(FleetDir(), "logs", fleetName+"-"+instanceName+".warn")
}

// WriteWarn writes warning to the instance's WarnPath. Errors are
// intentionally swallowed: warning files are best-effort surfacing of
// non-fatal failures during instance creation, and a write failure here
// must not itself fail the creation flow. Callers can assume the
// function returns immediately and never panics.
func WriteWarn(fleetName, instanceName, warning string) {
	_ = os.WriteFile(WarnPath(fleetName, instanceName), []byte(warning), 0644)
}

// Load reads the state from disk. Returns an empty state if the file doesn't exist.
func Load() (*State, error) {
	mu.Lock()
	defer mu.Unlock()

	s := &State{Fleets: make(map[string]*fleet.Fleet)}

	data, err := os.ReadFile(StatePath())
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	}

	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parsing state file: %w", err)
	}

	if s.Fleets == nil {
		s.Fleets = make(map[string]*fleet.Fleet)
	}
	if s.GroupLayouts == nil {
		s.GroupLayouts = make(map[string]GroupLayout)
	}

	return s, nil
}

// Save writes the state to disk.
func Save(s *State) error {
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(FleetDir(), 0755); err != nil {
		return fmt.Errorf("creating fleet dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	if err := os.WriteFile(StatePath(), data, 0644); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}

	return nil
}

// GetOrCreateFleet returns an existing fleet by name, or creates a new one with the given remote.
func (s *State) GetOrCreateFleet(name, remote string) *fleet.Fleet {
	if f, ok := s.Fleets[name]; ok {
		return f
	}
	f := &fleet.Fleet{
		Name:      name,
		Remote:    remote,
		Instances: make([]*fleet.Instance, 0),
	}
	s.Fleets[name] = f
	return f
}

// FindFleetByRemote finds a fleet by its remote URL.
func (s *State) FindFleetByRemote(remote string) *fleet.Fleet {
	for _, f := range s.Fleets {
		if f.Remote == remote {
			return f
		}
	}
	return nil
}

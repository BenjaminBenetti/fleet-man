package state

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
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
	if existing, ok := s.Fleets[name]; ok {
		return existing
	}
	newFleet := &fleet.Fleet{
		Name:      name,
		Remote:    remote,
		Instances: make([]*fleet.Instance, 0),
	}
	s.Fleets[name] = newFleet
	flog.Info("fleet created", "fleet", name, "remote", remote)
	return newFleet
}

// FindFleetByRemote finds a fleet by its remote URL.
func (s *State) FindFleetByRemote(remote string) *fleet.Fleet {
	for _, candidate := range s.Fleets {
		if candidate.Remote == remote {
			return candidate
		}
	}
	return nil
}

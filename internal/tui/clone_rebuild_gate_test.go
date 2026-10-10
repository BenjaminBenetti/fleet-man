package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// newGateModel builds a one-fleet model whose single provisioned, running
// instance uses backendType, with the cursor parked on that instance's row.
func newGateModel(t *testing.T, backendType fleet.BackendType) (*model, *fleetPage) {
	t.Helper()
	inst := &fleet.Instance{Name: "i1", Status: fleet.StatusRunning, Backend: backendType, ContainerID: "c1"}
	fp := newFleetPage()
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
		width:        100,
		height:       40,
	}
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.kind == rowInstance {
			fp.cursor = i
		}
	}
	if _, selected := fp.selectedInstance(m); selected != inst {
		t.Fatalf("cursor not on the instance row")
	}
	return m, fp
}

// TestCloneAndRebuildKeysFollowBackendCapabilities checks the C and R gates ask
// the backend type's capabilities rather than naming backends: each opens its
// dialog exactly when the capability is there, and otherwise says why.
func TestCloneAndRebuildKeysFollowBackendCapabilities(t *testing.T) {
	tests := []struct {
		backend     fleet.BackendType
		wantClone   bool
		wantRebuild bool
	}{
		{fleet.BackendDevcontainer, true, true},
		{"", true, true}, // records from before the Backend field
		{fleet.BackendCodespaces, false, true},
		{fleet.BackendCoder, false, false},
		// A type this TUI does not know is left to the server to judge.
		{"some-future-backend", true, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			m, fp := newGateModel(t, tt.backend)
			fp.updateNormal(m, key('C'))
			if got := fp.mode == viewCloneInstance; got != tt.wantClone {
				t.Errorf("C opened the clone dialog = %v, want %v (message %q)", got, tt.wantClone, m.message)
			}
			if !tt.wantClone && !strings.Contains(m.message, "Clone not supported") {
				t.Errorf("C message = %q, want a clone-not-supported notice", m.message)
			}

			m, fp = newGateModel(t, tt.backend)
			fp.updateNormal(m, key('R'))
			if got := fp.mode == viewConfirmRebuild; got != tt.wantRebuild {
				t.Errorf("R opened the rebuild confirm = %v, want %v (message %q)", got, tt.wantRebuild, m.message)
			}
			if !tt.wantRebuild && !strings.Contains(m.message, "Rebuild not supported") {
				t.Errorf("R message = %q, want a rebuild-not-supported notice", m.message)
			}
		})
	}
}

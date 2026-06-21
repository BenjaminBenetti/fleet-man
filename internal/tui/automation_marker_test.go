package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/lipgloss"
)

// TestAutomationMarkFitsReservedSlot guards the alignment contract: the marker
// plus its trailing space must render to exactly instanceAutoMarkWidth cells, or
// the status column (and the PR-status second line) drifts on automated rows.
func TestAutomationMarkFitsReservedSlot(t *testing.T) {
	if w := lipgloss.Width(automationMark + " "); w != instanceAutoMarkWidth {
		t.Fatalf("auto-mark %q renders %d cells, reserved slot is %d", automationMark, w, instanceAutoMarkWidth)
	}
}

// TestViewFleetListMarksAutomatedInstance verifies the ⟳ origin marker (issue
// #188) renders only on automation-spawned instance rows.
func TestViewFleetListMarksAutomatedInstance(t *testing.T) {
	auto := &fleet.Instance{Name: "nightly-run", Status: fleet.StatusRunning, Automated: true}
	manual := &fleet.Instance{Name: "my-fix", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{auto, manual}},
			},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
		width:        120,
	}
	fp.buildRows(m)
	view := fp.viewFleetList(m)

	var autoLine, manualLine string
	for _, ln := range strings.Split(view, "\n") {
		if strings.Contains(ln, "nightly-run") {
			autoLine = ln
		}
		if strings.Contains(ln, "my-fix") {
			manualLine = ln
		}
	}
	if autoLine == "" || manualLine == "" {
		t.Fatalf("both instance rows should render:\n%s", view)
	}
	if !strings.Contains(autoLine, automationMark) {
		t.Errorf("the automated instance row should carry the %q marker: %q", automationMark, autoLine)
	}
	if strings.Contains(manualLine, automationMark) {
		t.Errorf("the manual instance row must not carry the marker: %q", manualLine)
	}
}

// TestProtoInstanceToLegacyCarriesAutomated guards the TUI-side half of the
// instance wire mapping (the server-side half is covered in the server package).
func TestProtoInstanceToLegacyCarriesAutomated(t *testing.T) {
	if !protoInstanceToLegacy(&fleetgrpc.Instance{Name: "x", Automated: true}).Automated {
		t.Fatal("protoInstanceToLegacy should carry Automated=true")
	}
	if protoInstanceToLegacy(&fleetgrpc.Instance{Name: "x"}).Automated {
		t.Fatal("an absent automated flag should map to false")
	}
}

package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// TestAutomatedMarkerKeepsStatusAligned proves the trailing ⟳ doesn't shift the
// status column: an automated row and a differently-named manual row must land
// "running" at the same column (the marker lives inside the fixed name column).
func TestAutomatedMarkerKeepsStatusAligned(t *testing.T) {
	auto := &fleet.Instance{Name: "robo-job", Status: fleet.StatusRunning, Automated: true}
	manual := &fleet.Instance{Name: "a-longer-manual-name", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := &model{
		st: &state.State{Fleets: map[string]*fleet.Fleet{
			"alpha": {Name: "alpha", Instances: []*fleet.Instance{auto, manual}},
		}},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
		width:        120,
	}
	fp.buildRows(m)
	view := fp.viewFleetList(m)

	// Visual column (cell width), NOT byte index — the ⟳ is multi-byte, so a byte
	// offset would differ even when the rendered columns line up.
	statusCol := func(rowName string) int {
		for _, ln := range strings.Split(view, "\n") {
			if strings.Contains(ln, rowName) {
				plain := ansi.Strip(ln)
				if idx := strings.Index(plain, "running"); idx >= 0 {
					return lipgloss.Width(plain[:idx])
				}
				return -1
			}
		}
		t.Fatalf("row for %q not found:\n%s", rowName, view)
		return -1
	}
	a, b := statusCol("robo-job"), statusCol("a-longer-manual-name")
	if a < 0 || a != b {
		t.Fatalf("status column misaligned: automated=%d manual=%d", a, b)
	}
}

// TestAutomationMarkFitsNameColumn guards the alignment contract: the trailing
// marker (a leading space + the glyph) must render to exactly
// instanceAutoMarkWidth cells — the amount the name is truncated to make room —
// or the status column (and the PR-status second line) drifts on automated rows.
func TestAutomationMarkFitsNameColumn(t *testing.T) {
	if w := lipgloss.Width(" " + automationMark); w != instanceAutoMarkWidth {
		t.Fatalf("trailing auto-mark %q renders %d cells, name column reserves %d", automationMark, w, instanceAutoMarkWidth)
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

// TestAutomationLabelColorsFollowInstancePattern locks the hierarchy to mirror
// the instance view (fleet blue → instance white → session blue): the triggers/
// agents group header is white (not blue), while trigger/agent items keep the
// blue session color.
func TestAutomationLabelColorsFollowInstancePattern(t *testing.T) {
	blue := lipgloss.Color("39") // fleetExpandedStyle / sessionStyle foreground
	if automationGroupStyle.GetForeground() == blue {
		t.Error("triggers/agents group header should be white, not blue")
	}
	if sessionStyle.GetForeground() != blue {
		t.Error("trigger/agent items use sessionStyle, expected to be blue (color 39)")
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

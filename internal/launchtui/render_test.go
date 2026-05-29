package launchtui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// sampleCustomizations is a representative fleetLaunch block: a couple of links
// and a couple of apps, enough to exercise both sections and a wrapped row.
func sampleCustomizations() devcontainer.FleetCustomizations {
	return devcontainer.FleetCustomizations{
		FleetLaunch: devcontainer.FleetLaunchCustomizations{
			Sites: []devcontainer.FleetLaunchSite{
				{Title: "API", SubTitle: "REST backend", URL: "http://localhost:3000"},
				{Title: "Docs", SubTitle: "handbook", URL: "http://localhost:8080"},
				{Title: "Mail", URL: "http://localhost:1080"},
			},
			Apps: []devcontainer.FleetLaunchApp{
				{Title: "Grafana", Port: 3000},
				{Title: "Go", Port: 6060},
			},
		},
	}
}

// TestViewDoesNotLeakEscapeCodes is the regression guard for the rendering bug
// where styled squares were composed into a hand-rolled character grid that
// shredded their ANSI escape sequences, leaking fragments like "8;5;240;48;5;236m"
// onto the screen as visible text. It forces a colour profile (so lipgloss
// actually emits escapes even though `go test` has no TTY), renders the view,
// and asserts that once the *valid* escape sequences are stripped, none of the
// telltale SGR fragments remain as visible text.
func TestViewDoesNotLeakEscapeCodes(t *testing.T) {
	// Force colour output regardless of the test environment's TTY/profile so
	// the squares carry real escape sequences to (not) leak.
	lipgloss.SetColorProfile(termenv.ANSI256)

	m := newModel(buildItems(sampleCustomizations()), nil, nil)
	m.width = 90
	m.height = 30

	out := m.View()

	// With colour forced on, the output must contain genuine CSI sequences —
	// otherwise the test isn't actually exercising the styled path.
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("rendered view contains no ANSI escapes; colour profile not applied?\n%q", out)
	}

	// Stripping the valid escape sequences must remove every SGR code. If any of
	// these fragments survive as visible text, escapes leaked (the old bug).
	visible := ansi.Strip(out)
	for _, leak := range []string{"38;5", "48;5", "5;236", "5;238", "\x1b"} {
		if strings.Contains(visible, leak) {
			t.Errorf("escape fragment %q leaked into visible text:\n%s", leak, visible)
		}
	}

	// And the visible text must actually contain the section headers and the
	// item titles (subtitles are intentionally not rendered).
	for _, want := range []string{"Links", "Apps", "API", "Grafana"} {
		if !strings.Contains(visible, want) {
			t.Errorf("visible output is missing %q\nfull visible text:\n%s", want, visible)
		}
	}

	// Subtitles must NOT appear — the redesign shows title-only pills.
	for _, unwanted := range []string{"REST backend", "handbook"} {
		if strings.Contains(visible, unwanted) {
			t.Errorf("visible output unexpectedly contains subtitle %q", unwanted)
		}
	}
}

// TestViewGeometryMatchesLayout checks that the rendered rows line up with the
// geometry the mouse handler hit-tests against: the first link square's border
// must appear on the row layout() assigns to link 0, indented to its column.
func TestViewGeometryMatchesLayout(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii) // plain text: easiest to assert positions

	m := newModel(buildItems(sampleCustomizations()), nil, nil)
	m.width = 90
	m.height = 30

	lines := strings.Split(m.View(), "\n")
	gl := m.grid()
	if len(gl.placements) == 0 {
		t.Fatal("no placements")
	}

	// Link 0's pill: its label must appear on the row layout() assigns, indented
	// to its X plus the pill's left padding.
	top := gl.placements[0].rect.Y
	left := gl.placements[0].rect.X
	label := gl.placements[0].label
	if top >= len(lines) {
		t.Fatalf("layout puts link 0 at row %d but only %d rows rendered", top, len(lines))
	}
	// Strip escapes before measuring: lipgloss emits zero-width reset sequences
	// even under the Ascii profile, so the visible column is the rune index in
	// the stripped row, not the raw string.
	row := ansi.Strip(lines[top])
	wantIdx := left + pillPadding
	if idx := strings.Index(row, label); idx != wantIdx {
		t.Errorf("link 0 label %q at column %d, layout expected %d\nrow: %q", label, idx, wantIdx, row)
	}
}

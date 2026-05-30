package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Deps Check Page
// ===========================================

// depsCheckPage holds state for the first-startup dependency check screen.
type depsCheckPage struct {
	result []deps.Dependency
}

// newDepsCheckPage creates a new dependency check page with the
// given dependency results to display.
func newDepsCheckPage(result []deps.Dependency) *depsCheckPage {
	return &depsCheckPage{result: result}
}

// Init is called when the deps check page becomes active.
func (depsCheckPage *depsCheckPage) Init(m *model) tea.Cmd {
	return nil
}

// ===========================================
// Update
// ===========================================

// Update handles input on the first-startup dependency check screen.
func (depsCheckPage *depsCheckPage) Update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "esc", " ":
			depsCheckPage.result = nil
			return m.ChangeRoute(routeFleetList)
		case "q", "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit
		}
	}
	return nil
}

// ===========================================
// View
// ===========================================

// View renders the first-startup dependency check screen.
func (depsCheckPage *depsCheckPage) View(m *model) string {
	var b strings.Builder

	logo := "" +
		"  __ _         _\n" +
		" / _| |___ ___| |_\n" +
		"|  _| / -_) -_)  _|\n" +
		"|_| |_\\___\\___|\\___|"
	b.WriteString(renderGradient(logo))
	if version.Version != "" {
		b.WriteString(" " + dimStyle.Render(version.Version))
	}
	b.WriteString("\n\n")

	var lines []string
	lines = append(lines, dialogTitle.Render("Dependency Check"))
	lines = append(lines, "")

	for _, dep := range depsCheckPage.result {
		var status, note string
		if dep.Found {
			status = statusRunningStyle.Render("found")
		} else {
			status = errorStyle.Render("not found")
		}

		if !dep.Required {
			note = dimStyle.Render(" (optional)")
		}

		lines = append(lines, fmt.Sprintf("  %s  %s%s", status, dialogLabel.Render(dep.Name), note))

		if dep.Description != "" {
			lines = append(lines, fmt.Sprintf("    %s", dimStyle.Render(dep.Description)))
		}

		if !dep.Found {
			lines = append(lines, fmt.Sprintf("    Install: %s", dimStyle.Render(dep.InstallURL)))
		}
	}

	lines = append(lines, "")
	lines = append(lines, dialogHint.Render("[enter] Continue"))

	box := dialogBox.Width(80)
	if m.width > 0 && m.width-2 < 80 {
		box = dialogBox.Width(max(1, m.width-2))
	}
	b.WriteString(box.Render(strings.Join(lines, "\n")))
	b.WriteString("\n")

	return b.String()
}

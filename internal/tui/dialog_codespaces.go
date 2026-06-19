package tui

import (
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updateCodespacesAuth handles the dialog shown when gh is missing
// the "codespace" OAuth scope.
func (fleetPage *fleetPage) updateCodespacesAuth(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			fleetPage.mode = viewNormal
			m.message = "Launching GitHub auth..."
			return execProcess(
				exec.Command("gh", "auth", "login", "-h", "github.com", "-s", "codespace"),
				func(err error) tea.Msg {
					if err != nil {
						return execDoneMsg{fmt.Errorf("gh auth login: %w", err)}
					}
					return execDoneMsg{}
				},
			)
		case "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = "Auth cancelled — codespace creation requires the codespace scope"
		}
	}
	return nil
}

// updateCodespacesMachine handles the dialog shown when gh needs a
// machine type but none is configured.
func (fleetPage *fleetPage) updateCodespacesMachine(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			fleetPage.mode = viewNormal
			m.message = "Set the Machine field, then retry instance creation"
			return m.ChangeRoute(routeSettings)
		case "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = ""
		}
	}
	return nil
}

// updateCodespacesLimit handles the dialog shown when the user has
// hit the maximum codespace count.
func (fleetPage *fleetPage) updateCodespacesLimit(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = ""
		}
	}
	return nil
}

// ===========================================
// Port Forward Helpers
// ===========================================

// ===========================================
// Session Dialogs
// ===========================================

func (fleetPage *fleetPage) renderCodespacesAuthDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s\n\n%s",
		warnBanner.Render("  GitHub Auth Required  "),
		dialogLabel.Render(
			"GitHub CLI authentication with the \"codespace\" scope is\n"+
				"required. Press Enter to log in and grant the required scope.",
		),
		dimStyle.Render("gh auth login -h github.com -s codespace"),
		dialogHint.Render("[enter] Authenticate  [q/esc] Cancel"),
	)
	b.WriteString(warnBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderCodespacesMachineDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s",
		warnBanner.Render("  Machine Type Required  "),
		dialogLabel.Render(
			"GitHub Codespaces requires a machine type but none is\n"+
				"configured. Press Enter to open Settings and set one.",
		),
		dialogHint.Render("[enter] Open Settings  [q/esc] Cancel"),
	)
	b.WriteString(warnBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderCodespacesLimitDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s",
		warnBanner.Render("  Codespace Limit Reached  "),
		dialogLabel.Render(
			"You have started the maximum number of Codespaces.\n"+
				"Please stop some before creating a new instance,\n"+
				"or use a different instance backend.",
		),
		dialogHint.Render("[enter/q/esc] Dismiss"),
	)
	b.WriteString(warnBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

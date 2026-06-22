package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// updateConfirmDelete handles the instance/fleet deletion confirmation dialog.
func (fleetPage *fleetPage) updateConfirmDelete(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			if fleetPage.dlg.inst == "" {
				// Fleet-level delete — check if it has instances for double confirm
				if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok && len(f.Instances) > 0 {
					fleetPage.mode = viewConfirmDeleteFleetWarn
					return nil
				}
				// Empty fleet, just remove it
				delete(m.st.Fleets, fleetPage.dlg.fleet)
				delete(fleetPage.collapsed, fleetPage.dlg.fleet)
				_ = destroyFleetRemote(fleetPage.dlg.fleet)
				fleetPage.buildRows(m)
				m.message = fmt.Sprintf("Removed fleet %s", fleetPage.dlg.fleet)
			} else {
				// Instance-level delete runs as a server job. Flip an optimistic
				// in-memory Deleting status for the spinner (NOT persisted — the
				// server owns the teardown and the record removal).
				f, ok := m.st.Fleets[fleetPage.dlg.fleet]
				if ok {
					instance, err := f.GetInstance(fleetPage.dlg.inst)
					if err == nil {
						instance.Status = fleet.StatusDeleting
						fleetPage.buildRows(m)
						fleetPage.mode = viewNormal
						return deleteInstanceCmd(fleetPage.dlg.fleet, fleetPage.dlg.inst, m.portForwards)
					}
				}
			}
			fleetPage.mode = viewNormal

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmRebuild handles the instance rebuild confirmation dialog.
// Rebuild recreates the container in place (preserving the workspace), so it is
// a single-step confirm — no double warning like fleet delete.
func (fleetPage *fleetPage) updateConfirmRebuild(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			// Rebuild runs as a server job. Flip an optimistic in-memory
			// Rebuilding status for the spinner (NOT persisted — the server owns
			// the reprovision and the persisted status); operationDoneMsg reload()s
			// the authoritative result.
			f, ok := m.st.Fleets[fleetPage.dlg.fleet]
			if ok {
				instance, err := f.GetInstance(fleetPage.dlg.inst)
				if err == nil {
					instance.Status = fleet.StatusRebuilding
					fleetPage.buildRows(m)
					fleetPage.mode = viewNormal
					return rebuildInstanceCmd(fleetPage.dlg.fleet, fleetPage.dlg.inst)
				}
			}
			fleetPage.mode = viewNormal

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmDeleteFleetWarn handles the double-confirm dialog for
// fleets with running instances.
func (fleetPage *fleetPage) updateConfirmDeleteFleetWarn(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			f, ok := m.st.Fleets[fleetPage.dlg.fleet]
			if ok && len(f.Instances) > 0 {
				for _, instance := range f.Instances {
					instance.Status = fleet.StatusDeleting // optimistic, in-memory only
				}
				fleetPage.buildRows(m)
				fleetPage.mode = viewNormal
				return deleteFleetCmd(fleetPage.dlg.fleet, f.Instances, m.portForwards)
			} else if ok {
				delete(m.st.Fleets, fleetPage.dlg.fleet)
				delete(fleetPage.collapsed, fleetPage.dlg.fleet)
				_ = destroyFleetRemote(fleetPage.dlg.fleet)
				fleetPage.buildRows(m)
				m.message = fmt.Sprintf("Removed fleet %s", fleetPage.dlg.fleet)
			}
			fleetPage.mode = viewNormal

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmDeleteSession handles the session deletion confirmation dialog.
func (fleetPage *fleetPage) updateConfirmDeleteSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			fleetPage.mode = viewNormal
			ref := InstanceRef{Fleet: fleetPage.dlg.fleet, Instance: fleetPage.dlg.inst}
			f, ok := m.st.Fleets[fleetPage.dlg.fleet]
			if !ok {
				break
			}
			instance, err := f.GetInstance(fleetPage.dlg.inst)
			if err != nil {
				break
			}
			sanitized := SanitizeSessionName(instance.Name)
			if fleetPage.dlg.groupID != "" && isGroupedSession(sanitized, fleetPage.dlg.session) {
				return deleteGroupSessionsCmd(ref, sanitized, fleetPage.dlg.groupID)
			}
			return deleteSessionCmd(ref, fleetPage.dlg.session)

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmDeleteAutomation handles the trigger/agent deletion confirmation
// dialog. The actual removal + persistence stays in deleteTrigger/deleteAgent;
// this just gates them behind a yes.
func (fleetPage *fleetPage) updateConfirmDeleteAutomation(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "y", "Y", "enter":
		fleetPage.mode = viewNormal
		d := fleetPage.autoDel
		if d.kind == rowAgent {
			return fleetPage.deleteAgent(m, d.fleet, d.idx)
		}
		return fleetPage.deleteTrigger(m, d.fleet, d.idx)

	case "n", "N", "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		m.message = "Cancelled"
	}
	return nil
}

// ===========================================
// Backend Type Helpers
// ===========================================

func (fleetPage *fleetPage) renderConfirmDeleteDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	var title, body string
	if fleetPage.dlg.inst == "" {
		count := 0
		if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok {
			count = len(f.Instances)
		}
		title = "Delete fleet"
		body = fmt.Sprintf("Remove fleet %s and all %d instance(s)? This will stop all containers and delete all workspaces.", fleetPage.dlg.fleet, count)
	} else {
		title = "Delete instance"
		body = fmt.Sprintf("Remove %s/%s? This will stop the container and delete the workspace.", fleetPage.dlg.fleet, fleetPage.dlg.inst)
	}
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s",
		dialogTitle.Render(title),
		dialogLabel.Render(body),
		dialogHint.Render("[y] Yes  [n/q/esc] No"),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderConfirmRebuildDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	body := fmt.Sprintf("Rebuild %s/%s? This recreates the container from its devcontainer config. Your workspace — the git checkout and any uncommitted changes — is preserved.", fleetPage.dlg.fleet, fleetPage.dlg.inst)
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s",
		dialogTitle.Render("Rebuild instance"),
		dialogLabel.Render(body),
		dialogHint.Render("[y] Yes  [n/q/esc] No"),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderConfirmDeleteFleetWarnDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	count := 0
	if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok {
		count = len(f.Instances)
	}
	warnDialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s\n\n%s",
		warnBanner.Render("  !! WARNING !!  "),
		dialogLabel.Render(fmt.Sprintf(
			"You are about to destroy fleet %s with %d running instance(s).\nAll containers will be stopped and all workspace data will be permanently deleted.",
			fleetPage.dlg.fleet, count,
		)),
		errorStyle.Render("This action cannot be undone."),
		dialogHint.Render("[y] Confirm destroy  [n/q/esc] Cancel"),
	)
	b.WriteString(warnBox.Render(warnDialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderConfirmDeleteAutomationDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	noun := "trigger"
	if fleetPage.autoDel.kind == rowAgent {
		noun = "agent"
	}
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s",
		dialogTitle.Render("Delete "+noun),
		dialogLabel.Render(fmt.Sprintf("Remove %s %q from %s?",
			noun, fleetPage.autoDel.name, fleetPage.autoDel.fleet)),
		dialogHint.Render("[y] Yes  [n/q/esc] No"),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderConfirmDeleteSessionDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	displayName := fleetPage.dlg.session
	if fleetPage.dlg.groupID != "" {
		displayName = fleetPage.dlg.groupID
	}
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s",
		dialogTitle.Render("Delete session"),
		dialogLabel.Render(fmt.Sprintf("Remove session %s from %s/%s?",
			displayName, fleetPage.dlg.fleet, fleetPage.dlg.inst)),
		dialogHint.Render("[y] Yes  [n/q/esc] No"),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

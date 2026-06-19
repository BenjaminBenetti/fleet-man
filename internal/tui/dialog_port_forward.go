package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updatePortForward handles the port forward management dialog.
func (fleetPage *fleetPage) updatePortForward(m *model, msg tea.Msg) tea.Cmd {
	key := fleetPage.dlg.fleet + "/" + fleetPage.dlg.inst

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.addPortForward(m, key)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.closePortForward(m, key)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()

		case "d":
			fwds := m.portForwards.List(key)
			if len(fwds) > 0 && fleetPage.pfCursor < len(fwds) {
				fwd := fwds[fleetPage.pfCursor]
				_ = m.portForwards.Remove(key, fwd.LocalPort)
				m.message = fmt.Sprintf("Removed forward %s", fwd.Label())
				if fleetPage.pfCursor >= len(m.portForwards.List(key)) {
					fleetPage.pfCursor = max(0, fleetPage.pfCursor-1)
				}
				return nil
			}

		case "up", "k":
			if fleetPage.pfCursor > 0 {
				fleetPage.pfCursor--
			}
			return nil

		case "down", "j":
			fwds := m.portForwards.List(key)
			if fleetPage.pfCursor < len(fwds)-1 {
				fleetPage.pfCursor++
			}
			return nil

		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.closePortForward(m, key)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) addPortForward(m *model, key string) tea.Cmd {
	mappingInput := strings.TrimSpace(fleetPage.textInput.Value())
	if mappingInput == "" {
		return nil
	}
	local, remote, err := parsePortMapping(mappingInput)
	if err != nil {
		m.message = err.Error()
		return nil
	}

	// The server owns backend access: each accepted connection is tunnelled
	// to the instance over the server's Forward stream (the data plane), so
	// the forward works even against a remote server.
	dial, err := forwardDialer(fleetPage.dlg.fleet, fleetPage.dlg.inst, remote)
	if err != nil {
		m.message = err.Error()
		return nil
	}
	if err := m.portForwards.Add(key, local, remote, dial); err != nil {
		m.message = err.Error()
		return nil
	}

	fleetPage.textInput.SetValue("")
	m.message = fmt.Sprintf("Forwarding localhost:%d -> %s:%d", local, fleetPage.dlg.inst, remote)
	return nil
}

func (fleetPage *fleetPage) closePortForward(m *model, key string) tea.Cmd {
	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	fwds := m.portForwards.List(key)
	if len(fwds) > 0 {
		m.message = fmt.Sprintf("%d port forward(s) active on %s", len(fwds), fleetPage.dlg.inst)
	} else {
		m.message = ""
	}
	return nil
}

// parsePortMapping splits a "local:remote" string into two port numbers.
func parsePortMapping(raw string) (int, int, error) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected local:remote (e.g. 8080:80)")
	}
	local, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || local < 1 || local > 65535 {
		return 0, 0, fmt.Errorf("invalid local port %q", parts[0])
	}
	remote, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || remote < 1 || remote > 65535 {
		return 0, 0, fmt.Errorf("invalid remote port %q", parts[1])
	}
	return local, remote, nil
}

// ===========================================
// Codespaces Auth Scope Dialog
// ===========================================

func (fleetPage *fleetPage) portForwardHint() string {
	if fleetPage.dlg.fieldActive {
		return "[enter] Add  [esc] List  [ctrl+c] Close"
	}
	return "[j/k] Navigate  [d] Delete selected  [enter] Edit add field  [q/esc] Close"
}

// ===========================================
// Session Management
// ===========================================

func (fleetPage *fleetPage) renderPortForwardDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	pfKey := fleetPage.dlg.fleet + "/" + fleetPage.dlg.inst
	fwds := m.portForwards.List(pfKey)

	var fwdLines strings.Builder
	if len(fwds) == 0 {
		fwdLines.WriteString(dimStyle.Render("  No active forwards"))
	} else {
		for i, f := range fwds {
			pfCursor := "  "
			if i == fleetPage.pfCursor {
				pfCursor = cursorStyle.Render("> ")
			}
			fwdLines.WriteString(fmt.Sprintf("%s%s\n",
				pfCursor,
				portForwardStyle.Render(f.Label()),
			))
		}
	}

	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n\n%s\n\n%s %s\n\n%s",
		dialogTitle.Render("Port forwards"),
		dialogLabel.Render("Instance:"),
		fleetExpandedStyle.Render(fleetPage.dlg.fleet+"/"+fleetPage.dlg.inst),
		strings.TrimRight(fwdLines.String(), "\n"),
		dialogLabel.Render("Add:"),
		fleetPage.textInput.View(),
		dialogHint.Render(fleetPage.portForwardHint()),
	)
	b.WriteString(portForwardBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

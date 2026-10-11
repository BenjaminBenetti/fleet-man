package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/editorlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	tea "github.com/charmbracelet/bubbletea"
)

// editor_launch.go is the TUI's `c`: open VS Code on the selected instance
// the way the daemon says (editorlaunch, the same path as `fleet code`). For
// VS Code Remote-SSH the first open asks before fleet adds `Include
// fleet_config` to ~/.ssh/config (the consent dialog); with the line there,
// the dialog is not shown again.

// vscodeResolveTimeout bounds asking the daemon (and the local daemon, for
// a remote's record) how to open the editor.
const vscodeResolveTimeout = 20 * time.Second

// vscodeResolvedMsg is how to open VS Code on an instance, or why not.
type vscodeResolvedMsg struct {
	name   string // the instance's display name
	launch editorlaunch.Launch
	err    error
}

// vscodeOpenedMsg reports one VS Code launch.
type vscodeOpenedMsg struct {
	name string
	err  error
}

// vscodeConsentState is the consent dialog's (mode == viewEditorConsent).
type vscodeConsentState struct {
	name   string
	launch editorlaunch.Launch
	row    int
}

// The consent dialog's choices, in order.
const (
	vscodeConsentAllow = iota
	vscodeConsentManual
	vscodeConsentCancel
	vscodeConsentChoices
)

// resolveVSCode asks the daemon the TUI is connected to how to open VS Code
// on instance (a var so tests can stand in a daemon).
var resolveVSCode = func(fleetName, instance string) (editorlaunch.Launch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), vscodeResolveTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return editorlaunch.Launch{}, err
	}
	return editorlaunch.Resolve(ctx, editorlaunch.Services{Target: conn.Service(), Local: editorlaunch.LocalDaemon}, fleetName, instance)
}

// runVSCode runs a launch's command (a var for tests).
var runVSCode = func(l editorlaunch.Launch) error {
	if l.RemoteSSH() {
		if err := editorlaunch.CheckRemoteSSH(l.Argv[0]); err != nil {
			return err
		}
	}
	return exec.Command(l.Argv[0], l.Argv[1:]...).Run()
}

// addVSCodeInclude adds the Include line (a var for tests).
var addVSCodeInclude = editorlaunch.AddInclude

// openVSCodeCmd resolves how to open VS Code on instance, off the update
// loop.
func openVSCodeCmd(fleetName, instance, name string) tea.Cmd {
	return func() tea.Msg {
		l, err := resolveVSCode(fleetName, instance)
		return vscodeResolvedMsg{name: name, launch: l, err: err}
	}
}

// launchVSCodeCmd runs a launch, off the update loop.
func launchVSCodeCmd(name string, l editorlaunch.Launch) tea.Cmd {
	return func() tea.Msg {
		return vscodeOpenedMsg{name: name, err: runVSCode(l)}
	}
}

// handleVSCodeResolved opens VS Code, or first the consent dialog.
func (fleetPage *fleetPage) handleVSCodeResolved(m *model, msg vscodeResolvedMsg) tea.Cmd {
	if msg.err != nil {
		m.message = fmt.Sprintf("VS Code error: %v", msg.err)
		return nil
	}
	if msg.launch.RemoteSSH() && msg.launch.NeedsInclude {
		if fleetPage.mode != viewNormal {
			// Another dialog opened while the daemon answered: leave it be.
			m.message = fmt.Sprintf("VS Code for %s needs consent to add `%s` to ~/.ssh/config: press c again", msg.name, sshalias.IncludeLine)
			return nil
		}
		fleetPage.vscodeDlg = vscodeConsentState{name: msg.name, launch: msg.launch}
		fleetPage.mode = viewEditorConsent
		return nil
	}
	m.message = fmt.Sprintf("Opening VS Code for %s…", msg.name)
	return launchVSCodeCmd(msg.name, msg.launch)
}

func (fleetPage *fleetPage) handleVSCodeOpened(m *model, msg vscodeOpenedMsg) tea.Cmd {
	if msg.err != nil {
		m.message = fmt.Sprintf("VS Code error: %v", msg.err)
	} else {
		m.message = fmt.Sprintf("Opened VS Code for %s", msg.name)
	}
	return nil
}

// updateEditorConsent handles the consent dialog: j/k (or tab) choose,
// enter acts, a/m are shortcuts, esc cancels.
func (fleetPage *fleetPage) updateEditorConsent(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	dlg := &fleetPage.vscodeDlg
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		return fleetPage.chooseEditorConsent(m, vscodeConsentCancel)
	case "up", "k", "left", "h", "shift+tab":
		dlg.row = (dlg.row - 1 + vscodeConsentChoices) % vscodeConsentChoices
	case "down", "j", "right", "l", "tab":
		dlg.row = (dlg.row + 1) % vscodeConsentChoices
	case "enter", " ":
		return fleetPage.chooseEditorConsent(m, dlg.row)
	case "a", "A":
		return fleetPage.chooseEditorConsent(m, vscodeConsentAllow)
	case "m", "M":
		return fleetPage.chooseEditorConsent(m, vscodeConsentManual)
	}
	return nil
}

func (fleetPage *fleetPage) chooseEditorConsent(m *model, choice int) tea.Cmd {
	dlg := fleetPage.vscodeDlg
	fleetPage.mode = viewNormal
	fleetPage.vscodeDlg = vscodeConsentState{}
	switch choice {
	case vscodeConsentAllow:
		if err := addVSCodeInclude(); err != nil {
			m.message = fmt.Sprintf("VS Code error: could not add the Include to ~/.ssh/config: %v", err)
			return nil
		}
		m.message = fmt.Sprintf("Opening VS Code for %s…", dlg.name)
		return launchVSCodeCmd(dlg.name, dlg.launch)
	case vscodeConsentManual:
		m.message = editorlaunch.ManualSteps(dlg.launch)
	default:
		m.message = "Cancelled: ~/.ssh/config is unchanged"
	}
	return nil
}

// renderEditorConsentDialog is the consent dialog.
func (fleetPage *fleetPage) renderEditorConsentDialog(*model) string {
	opt := func(r int, label string) string {
		if fleetPage.vscodeDlg.row == r {
			return cursorStyle.Render("> ") + selectedStyle.Render(label)
		}
		return "  " + dialogLabel.Render(label)
	}
	body := strings.Join([]string{
		"Kubernetes and remote instances open via VS Code Remote-SSH.",
		"Fleet will add one line to ~/.ssh/config:",
		"    " + sshalias.IncludeLine,
		"(hosts " + sshalias.HostPattern + " route through `fleet " + sshalias.ProxySubcommand + "`)",
		"Extensions from devcontainer.json install on first connect.",
	}, "\n")
	dialog := fmt.Sprintf("%s\n\n%s\n\n%s  %s  %s\n\n%s",
		dialogTitle.Render("Open in VS Code"),
		dialogLabel.Render(body),
		opt(vscodeConsentAllow, "[ Allow and open ]"),
		opt(vscodeConsentManual, "[ Show manual steps ]"),
		opt(vscodeConsentCancel, "[ Cancel ]"),
		dialogHint.Render("[←/→] Select  [enter] Choose  [a] Allow  [m] Manual steps  [esc] Cancel"),
	)
	return "\n" + dialogBox.Render(dialog) + "\n"
}

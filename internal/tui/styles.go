package tui

import (
	"github.com/BenjaminBenetti/fleet-man/internal/theme"
	"github.com/charmbracelet/lipgloss"
)

// The TUI's styles are package-level so every renderer can reach them without
// threading a style set through; applyTheme rebuilds all of them from a
// theme.Theme (issue #251). Nothing the TUI DRAWS names a raw color — every
// color comes from a theme slot, so a theme switch is one call and a slot
// renamed here is a compile error, not a stray hardcoded shade. (The tmux
// status bar fleet configures INSIDE instance sessions, dotfiles.go, keeps
// its own colours: that is pane content, which themes leave alone.)
//
// Callers that build a one-off style (a dialog frame sized to the terminal)
// read the slot off activeTheme instead.

// activeTheme is the theme the styles below were last built from.
var activeTheme theme.Theme

var (
	titleStyle lipgloss.Style

	waveStyle lipgloss.Style

	// List box
	listBox lipgloss.Style

	// Scrollbar (settings page viewport)
	scrollbarThumbStyle lipgloss.Style
	scrollbarTrackStyle lipgloss.Style
	scrollbarArrowStyle lipgloss.Style

	// Fleet header
	fleetExpandedStyle  lipgloss.Style
	fleetCollapsedStyle lipgloss.Style

	// Selection
	selectedStyle lipgloss.Style
	cursorStyle   lipgloss.Style

	// Instance details
	dimStyle lipgloss.Style

	// automationMarkStyle colors the ⟳ marker on automation-spawned instances
	// (issue #188): a calm cyan that reads as a distinct origin badge without
	// competing with the status word or the agent indicators.
	automationMarkStyle lipgloss.Style

	statusRunningStyle  lipgloss.Style
	statusStoppedStyle  lipgloss.Style
	statusCreatingStyle lipgloss.Style

	// Auto-tag (PR status) signal colours, reusing the running/creating/error
	// palette: green = good, yellow = in-progress/neutral, red = needs attention.
	// Grey de-emphasises the pending/in-progress states (review "Pending" and
	// pending checks) to cut visual noise.
	prGreenStyle  lipgloss.Style
	prYellowStyle lipgloss.Style
	prRedStyle    lipgloss.Style
	prGrayStyle   lipgloss.Style

	// Purple marks a closed/merged PR, matching GitHub's own colour. The tag is
	// kept visible (rather than vanishing) so a finished instance is
	// distinguishable from one that never had a PR.
	prPurpleStyle lipgloss.Style

	// Agent tool indicator
	agentWorkingStyle lipgloss.Style
	agentWaitingStyle lipgloss.Style
	agentOffStyle     lipgloss.Style

	// Help bar
	helpStyle lipgloss.Style

	// Message
	messageStyle lipgloss.Style

	errorStyle lipgloss.Style

	// Dialog box
	dialogBox   lipgloss.Style
	dialogTitle lipgloss.Style
	dialogLabel lipgloss.Style
	dialogHint  lipgloss.Style

	warnBox    lipgloss.Style
	warnBanner lipgloss.Style

	// Port forward dialog
	portForwardBox   lipgloss.Style
	portForwardStyle lipgloss.Style

	// Session rows
	sessionStyle       lipgloss.Style
	sessionActiveStyle lipgloss.Style
	newSessionStyle    lipgloss.Style

	// Automation view group labels (triggers/agents). They mirror the instance
	// view's hierarchy: fleet header blue, the group white like an instance name,
	// and the trigger/agent items under it blue (sessionStyle) like sessions. Bold
	// weight reads as a group header.
	automationGroupStyle = lipgloss.NewStyle().Bold(true)

	// Keybindings dialog
	keybindingsDialogBox   lipgloss.Style
	keybindingSectionStyle lipgloss.Style
	keybindingKeyStyle     lipgloss.Style
	keybindingDescStyle    lipgloss.Style

	// Update notification
	updateStyle lipgloss.Style

	// Spinner (the page spinner; the agent throbber reuses agentWorkingStyle)
	spinnerStyle lipgloss.Style
)

func init() {
	applyTheme(theme.Fleet())
}

// applyTheme rebuilds every package-level style (and the instance color cycle)
// from t. Renderers pick the styles up on their next View, so the caller only
// has to refresh what it copied out of them (the two spinners' styles; see
// model.setTheme).
func applyTheme(t theme.Theme) {
	activeTheme = t

	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent).
		PaddingBottom(1)

	waveStyle = lipgloss.NewStyle().
		Foreground(t.Primary)

	listBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Padding(0, 1)

	scrollbarThumbStyle = lipgloss.NewStyle().Foreground(t.Border)
	scrollbarTrackStyle = lipgloss.NewStyle().Foreground(t.Faint)
	scrollbarArrowStyle = lipgloss.NewStyle().Foreground(t.Border)

	fleetExpandedStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Primary)

	fleetCollapsedStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Collapsed)

	selectedStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	cursorStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	dimStyle = lipgloss.NewStyle().
		Foreground(t.Muted)

	automationMarkStyle = lipgloss.NewStyle().
		Foreground(t.Info)

	statusRunningStyle = lipgloss.NewStyle().
		Foreground(t.Success)

	statusStoppedStyle = lipgloss.NewStyle().
		Foreground(t.Primary)

	statusCreatingStyle = lipgloss.NewStyle().
		Foreground(t.Warning)

	prGreenStyle = lipgloss.NewStyle().
		Foreground(t.Success)

	prYellowStyle = lipgloss.NewStyle().
		Foreground(t.Warning)

	prRedStyle = lipgloss.NewStyle().
		Foreground(t.Error)

	prGrayStyle = lipgloss.NewStyle().
		Foreground(t.Muted)

	prPurpleStyle = lipgloss.NewStyle().
		Foreground(t.Purple)

	agentWorkingStyle = lipgloss.NewStyle().
		Foreground(t.Success).
		Bold(true)

	agentWaitingStyle = lipgloss.NewStyle().
		Foreground(t.Warning).
		Bold(true)

	agentOffStyle = lipgloss.NewStyle().
		Foreground(t.Muted)

	helpStyle = lipgloss.NewStyle().
		Foreground(t.Subtle).
		PaddingTop(1)

	messageStyle = lipgloss.NewStyle().
		Foreground(t.Message).
		PaddingTop(1)

	errorStyle = lipgloss.NewStyle().
		Foreground(t.Error).
		Bold(true)

	dialogBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Accent).
		Padding(1, 2).
		Width(50)

	dialogTitle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	dialogLabel = lipgloss.NewStyle().
		Foreground(t.Text)

	dialogHint = lipgloss.NewStyle().
		Foreground(t.Subtle).
		PaddingTop(1)

	warnBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Error).
		Padding(1, 2).
		Width(50)

	warnBanner = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Error).
		Background(t.ErrorBg)

	portForwardBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Primary).
		Padding(1, 2).
		Width(55)

	portForwardStyle = lipgloss.NewStyle().
		Foreground(t.Success)

	sessionStyle = lipgloss.NewStyle().
		Foreground(t.Primary)

	sessionActiveStyle = lipgloss.NewStyle().
		Foreground(t.Success).
		Bold(true)

	newSessionStyle = lipgloss.NewStyle().
		Foreground(t.Subtle)

	keybindingsDialogBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Primary).
		Padding(1, 2).
		Width(106)

	keybindingSectionStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Primary)

	keybindingKeyStyle = lipgloss.NewStyle().
		Foreground(t.Accent).
		Bold(true).
		Width(20)

	keybindingDescStyle = lipgloss.NewStyle().
		Foreground(t.Text)

	updateStyle = lipgloss.NewStyle().
		Foreground(t.Warning).
		Bold(true)

	spinnerStyle = lipgloss.NewStyle().Foreground(t.Accent)

	instanceColors = buildInstanceColors(t.Instance)
}

// framedBox is a rounded-border frame in the given color, for the dialogs
// that size themselves to the terminal at render time.
func framedBox(border lipgloss.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(1, 2)
}

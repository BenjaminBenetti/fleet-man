package tui

import "github.com/charmbracelet/lipgloss"

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170")).
			PaddingBottom(1)

	waveStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39"))

	// List box
	listBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(0, 1)

	// Scrollbar (settings page viewport)
	scrollbarThumbStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	scrollbarTrackStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	scrollbarArrowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))

	// Fleet header
	fleetExpandedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("39"))

	fleetCollapsedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("245"))

	// Selection
	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	cursorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	// Instance details
	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	statusRunningStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("42"))

	statusStoppedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("39"))

	statusCreatingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("214"))

	// Auto-tag (PR status) signal colours, reusing the running/creating/error
	// palette: green = good, yellow = in-progress/neutral, red = needs attention.
	prGreenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("42"))

	prYellowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))

	prRedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	// Agent tool indicator
	agentWorkingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("42")).
				Bold(true)

	agentWaitingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("214")).
				Bold(true)

	agentOffStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	// Help bar
	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			PaddingTop(1)

	// Message
	messageStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("229")).
			PaddingTop(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	// Dialog box
	dialogBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("170")).
			Padding(1, 2).
			Width(50)

	dialogTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	dialogLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	dialogHint = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			PaddingTop(1)

	warnBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("196")).
		Padding(1, 2).
		Width(50)

	warnBanner = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("196")).
			Background(lipgloss.Color("52"))

	// Port forward dialog
	portForwardBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("39")).
			Padding(1, 2).
			Width(55)

	portForwardStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("42"))

	// Session rows
	sessionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39"))

	sessionActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("42")).
				Bold(true)

	newSessionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	// Keybindings dialog
	keybindingsDialogBox = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("39")).
				Padding(1, 2).
				Width(106)

	keybindingSectionStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("39"))

	keybindingKeyStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("170")).
				Bold(true).
				Width(20)

	keybindingDescStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))

	// Update notification
	updateStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).
			Bold(true)
)

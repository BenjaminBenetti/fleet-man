package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/agent"
	"github.com/BenjaminBenetti/fleet-man/internal/doctor"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Settings Item Constants
// ===========================================

const (
	settingsItemToolSelection = iota
	settingsItemTmuxVimKeys
	settingsItemShowHelpText
	settingsItemUpdate // only visible when an update is available
	settingsItemDotfilesRepo
	settingsItemDotfilesScript
	settingsItemDotfilesAutoInstall
	settingsItemDotfilesSetup
	settingsItemCoderTemplate
	settingsItemCoderPreset
	settingsItemCoderParamBase // parameters are at index base + i

	settingsItemCodespacesMachine = 500 // codespaces settings start here

	settingsItemBrowserMultiple   = 600 // browser settings start here
	settingsItemBrowserAutoSwitch = 601

	settingsItemToolStatusBase = 1000 // tool status rows start here
	settingsItemDoctor         = 2000 // doctor action row
	settingsItemKeybindings    = 2001 // keybindings dialog row
)

// toolStatusCount is the number of rows rendered in the Tool Status
// section. Must match the length of deps.CheckTools().
const toolStatusCount = 5

// dotfilesSetupPrompt is the instruction sent to the coding agent for
// guided dotfiles setup.
const dotfilesSetupPrompt = "Follow the instructions in https://raw.githubusercontent.com/BenjaminBenetti/Teeleport/main/SETUP_SKILL.md to help me set up Teeleport."

// ===========================================
// Settings Page
// ===========================================

// settingsPage holds settings-page-specific state.
type settingsPage struct {
	cursor          int
	editing         bool
	input           textinput.Model
	showKeybindings bool

	// itemRowYs maps item ID -> terminal Y where the item's first line
	// is rendered. itemHeights maps item ID -> number of lines the item
	// occupies. Both are populated during View() so mouse clicks can be
	// mapped back to the item under the cursor.
	itemRowYs   map[int]int
	itemHeights map[int]int
}

// newSettingsPage creates a new settings page with default state.
func newSettingsPage() *settingsPage {
	input := textinput.New()
	input.CharLimit = 256
	return &settingsPage{
		input:       input,
		itemRowYs:   make(map[int]int),
		itemHeights: make(map[int]int),
	}
}

// Init is called when the settings page becomes active.
func (settingsPage *settingsPage) Init(m *model) tea.Cmd {
	return nil
}

// Update dispatches settings page messages to the appropriate handler.
func (settingsPage *settingsPage) Update(m *model, msg tea.Msg) tea.Cmd {
	if settingsPage.showKeybindings {
		return settingsPage.updateKeybindingsDialog(m, msg)
	}
	if settingsPage.editing {
		return settingsPage.updateSettingsEditing(m, msg)
	}
	return settingsPage.updateSettingsNav(m, msg)
}

// View renders the settings page.
func (settingsPage *settingsPage) View(m *model) string {
	return settingsPage.viewSettings(m)
}

// ===========================================
// Settings Sections
// ===========================================

// settingsSection defines a titled group of settings rows that can be
// conditionally shown based on tool availability.
type settingsSection struct {
	Title string                           // section header text
	Tool  string                           // required tool binary; "" = always visible
	Items func(config *state.Config) []int // returns navigable item IDs for this section
}

// settingsSections lists all settings sections in display order.
var settingsSections = []settingsSection{
	{
		Title: "General",
		Items: func(_ *state.Config) []int {
			return []int{settingsItemTmuxVimKeys, settingsItemShowHelpText, settingsItemUpdate}
		},
	},
	{
		Title: "Dotfiles",
		Items: func(_ *state.Config) []int {
			return []int{settingsItemDotfilesRepo, settingsItemDotfilesScript, settingsItemDotfilesAutoInstall, settingsItemDotfilesSetup}
		},
	},
	{
		Title: "Coder",
		Tool:  "coder",
		Items: func(config *state.Config) []int {
			items := []int{settingsItemCoderTemplate, settingsItemCoderPreset}
			if config != nil {
				for i := range config.CoderSettings.Parameters {
					items = append(items, settingsItemCoderParamBase+i)
				}
			}
			return items
		},
	},
	{
		Title: "Codespaces",
		Tool:  "gh",
		Items: func(_ *state.Config) []int {
			return []int{settingsItemCodespacesMachine}
		},
	},
	{
		Title: "Browser",
		Items: func(config *state.Config) []int {
			items := []int{settingsItemBrowserMultiple}
			// Auto-switch only applies in shared-profile mode — in
			// per-instance mode there is no "switch" to suppress.
			if config != nil && !config.BrowserSettings.MultipleBrowsersPerFleetEnabled() {
				items = append(items, settingsItemBrowserAutoSwitch)
			}
			return items
		},
	},
	{
		Title: "Tool Status",
		Items: func(_ *state.Config) []int {
			items := make([]int, toolStatusCount)
			for i := range items {
				items[i] = settingsItemToolStatusBase + i
			}
			return items
		},
	},
	{
		Title: "Help",
		Items: func(_ *state.Config) []int {
			return []int{settingsItemDoctor, settingsItemKeybindings}
		},
	},
}

// ===========================================
// Navigation Helpers
// ===========================================

// sectionVisible reports whether a settings section should be shown.
func (settingsPage *settingsPage) sectionVisible(m *model, section settingsSection) bool {
	if section.Tool == "" {
		return true
	}
	for _, tool := range m.toolStatus {
		if tool.Binary == section.Tool {
			return tool.Found
		}
	}
	return false
}

// visibleItems returns the flat ordered list of navigable item IDs.
func (settingsPage *settingsPage) visibleItems(m *model) []int {
	var items []int
	for _, section := range settingsSections {
		if !settingsPage.sectionVisible(m, section) {
			continue
		}
		for _, id := range section.Items(m.config) {
			if id == settingsItemUpdate && m.updateAvailable == "" {
				continue
			}
			items = append(items, id)
		}
	}
	return items
}

// settingsCursorItem returns the item ID at the current cursor position.
func (settingsPage *settingsPage) settingsCursorItem(m *model) int {
	items := settingsPage.visibleItems(m)
	if settingsPage.cursor >= 0 && settingsPage.cursor < len(items) {
		return items[settingsPage.cursor]
	}
	return -1
}

// settingsItemCount returns the total number of navigable settings rows.
func (settingsPage *settingsPage) settingsItemCount(m *model) int {
	return len(settingsPage.visibleItems(m))
}

// ===========================================
// Toggle Helpers
// ===========================================

// toggleTmuxVimKeys toggles the tmux vim keys setting.
func (settingsPage *settingsPage) toggleTmuxVimKeys(m *model) {
	if m.config == nil {
		m.config = state.DefaultConfig()
	}
	current := m.config.GeneralSettings.TmuxVimKeysEnabled()
	next := !current
	m.config.GeneralSettings.TmuxVimKeys = &next
	if err := state.SaveConfig(m.config); err != nil {
		m.config.GeneralSettings.TmuxVimKeys = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Tmux vim keys set to %s", label)
}

// toggleShowHelpText flips the show-help-text preference and saves.
func (settingsPage *settingsPage) toggleShowHelpText(m *model) {
	if m.config == nil {
		m.config = state.DefaultConfig()
	}
	current := m.config.GeneralSettings.ShowHelpTextEnabled()
	next := !current
	m.config.GeneralSettings.ShowHelpText = &next
	if err := state.SaveConfig(m.config); err != nil {
		m.config.GeneralSettings.ShowHelpText = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Show help text set to %s", label)
}

// toggleBrowserAutoSwitch flips the "Auto Switch" preference and saves.
// When on, the browser-switch confirmation dialog is suppressed and any
// running browser bound to the target data dir is killed+relaunched
// silently.
func (settingsPage *settingsPage) toggleBrowserAutoSwitch(m *model) {
	if m.config == nil {
		m.config = state.DefaultConfig()
	}
	current := m.config.BrowserSettings.AutoSwitchEnabled()
	next := !current
	m.config.BrowserSettings.AutoSwitch = &next
	if err := state.SaveConfig(m.config); err != nil {
		m.config.BrowserSettings.AutoSwitch = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Auto switch set to %s", label)
}

// toggleBrowserMultiple flips the "Enable Multiple Browsers Per Fleet"
// preference and saves. When on, each instance gets its own browser
// data dir under <fleet>/<instance>/.browser instead of sharing a
// single profile under <fleet>/.browser.
func (settingsPage *settingsPage) toggleBrowserMultiple(m *model) {
	if m.config == nil {
		m.config = state.DefaultConfig()
	}
	current := m.config.BrowserSettings.MultipleBrowsersPerFleetEnabled()
	next := !current
	m.config.BrowserSettings.MultipleBrowsersPerFleet = &next
	if err := state.SaveConfig(m.config); err != nil {
		m.config.BrowserSettings.MultipleBrowsersPerFleet = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Multiple browsers per fleet set to %s", label)
}

// toggleAutoInstall toggles the dotfiles auto-install setting.
func (settingsPage *settingsPage) toggleAutoInstall(m *model) {
	if m.config == nil {
		m.config = state.DefaultConfig()
	}
	m.config.DotfilesSettings.AutoInstall = !m.config.DotfilesSettings.AutoInstall
	if err := state.SaveConfig(m.config); err != nil {
		m.config.DotfilesSettings.AutoInstall = !m.config.DotfilesSettings.AutoInstall
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if m.config.DotfilesSettings.AutoInstall {
		label = "on"
	}
	m.message = fmt.Sprintf("Auto install dotfiles set to %s", label)
}

// cycleCoderPreset cycles through available coder presets.
func (settingsPage *settingsPage) cycleCoderPreset(m *model, direction int) {
	if m.config == nil || len(m.coderPresets) == 0 {
		return
	}
	current := m.config.CoderSettings.Preset
	idx := 0
	for i, preset := range m.coderPresets {
		if preset == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(m.coderPresets)) % len(m.coderPresets)
	m.config.CoderSettings.Preset = m.coderPresets[idx]
	if err := state.SaveConfig(m.config); err != nil {
		m.config.CoderSettings.Preset = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	m.message = fmt.Sprintf("Preset set to %s", m.config.CoderSettings.Preset)
}

// cycleCodespacesMachine cycles through available codespace machine types.
func (settingsPage *settingsPage) cycleCodespacesMachine(m *model, direction int) {
	if m.config == nil || len(m.codespaceMachines) == 0 {
		return
	}
	current := m.config.CodespacesSettings.Machine
	idx := 0
	for i, machine := range m.codespaceMachines {
		if machine.Name == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(m.codespaceMachines)) % len(m.codespaceMachines)
	selected := m.codespaceMachines[idx]
	m.config.CodespacesSettings.Machine = selected.Name
	if err := state.SaveConfig(m.config); err != nil {
		m.config.CodespacesSettings.Machine = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	m.message = fmt.Sprintf("Machine set to %s", selected.DisplayName)
}

// codespacesMachineLabel returns the display label for the currently
// configured machine.
func (settingsPage *settingsPage) codespacesMachineLabel(m *model) string {
	name := m.config.CodespacesSettings.Machine
	for _, machine := range m.codespaceMachines {
		if machine.Name == name {
			return machine.DisplayName
		}
	}
	return name
}

// ===========================================
// Update Handlers
// ===========================================

// updateKeybindingsDialog handles input while the keybindings overlay is shown.
func (settingsPage *settingsPage) updateKeybindingsDialog(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q", "ctrl+c":
			settingsPage.showKeybindings = false
		}
	}
	return nil
}

// updateSettingsNav handles keyboard navigation in the settings page.
func (settingsPage *settingsPage) updateSettingsNav(m *model, msg tea.Msg) tea.Cmd {
	count := settingsPage.settingsItemCount(m)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = ""

		switch msg.String() {
		case "esc", "q":
			return m.ChangeRoute(routeFleetList)

		case "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit

		case "up", "k":
			settingsPage.cursor = (settingsPage.cursor - 1 + count) % count
			return nil

		case "down", "j":
			settingsPage.cursor = (settingsPage.cursor + 1) % count
			return nil

		case "left", "h":
			item := settingsPage.settingsCursorItem(m)
			if item == settingsItemTmuxVimKeys {
				settingsPage.toggleTmuxVimKeys(m)
			} else if item == settingsItemShowHelpText {
				settingsPage.toggleShowHelpText(m)
			} else if item == settingsItemDotfilesAutoInstall {
				settingsPage.toggleAutoInstall(m)
			} else if item == settingsItemBrowserMultiple {
				settingsPage.toggleBrowserMultiple(m)
			} else if item == settingsItemBrowserAutoSwitch {
				settingsPage.toggleBrowserAutoSwitch(m)
			} else if item == settingsItemCoderPreset {
				settingsPage.cycleCoderPreset(m, -1)
			} else if item == settingsItemCodespacesMachine {
				settingsPage.cycleCodespacesMachine(m, -1)
			}
			return nil

		case "right", "l":
			item := settingsPage.settingsCursorItem(m)
			if item == settingsItemTmuxVimKeys {
				settingsPage.toggleTmuxVimKeys(m)
			} else if item == settingsItemShowHelpText {
				settingsPage.toggleShowHelpText(m)
			} else if item == settingsItemDotfilesAutoInstall {
				settingsPage.toggleAutoInstall(m)
			} else if item == settingsItemBrowserMultiple {
				settingsPage.toggleBrowserMultiple(m)
			} else if item == settingsItemBrowserAutoSwitch {
				settingsPage.toggleBrowserAutoSwitch(m)
			} else if item == settingsItemCoderPreset {
				settingsPage.cycleCoderPreset(m, 1)
			} else if item == settingsItemCodespacesMachine {
				settingsPage.cycleCodespacesMachine(m, 1)
			}
			return nil

		case "enter", " ":
			item := settingsPage.settingsCursorItem(m)
			if item == settingsItemTmuxVimKeys {
				settingsPage.toggleTmuxVimKeys(m)
				return nil
			}
			if item == settingsItemShowHelpText {
				settingsPage.toggleShowHelpText(m)
				return nil
			}
			if item == settingsItemDotfilesAutoInstall {
				settingsPage.toggleAutoInstall(m)
				return nil
			}
			if item == settingsItemBrowserMultiple {
				settingsPage.toggleBrowserMultiple(m)
				return nil
			}
			if item == settingsItemBrowserAutoSwitch {
				settingsPage.toggleBrowserAutoSwitch(m)
				return nil
			}
			if item == settingsItemCoderPreset {
				settingsPage.cycleCoderPreset(m, 1)
			}
			if item == settingsItemCodespacesMachine {
				settingsPage.cycleCodespacesMachine(m, 1)
				return nil
			}
			if item == settingsItemUpdate {
				return performUpdateCmd()
			}
			if item == settingsItemDoctor {
				cmd, err := doctor.Command()
				if err != nil {
					m.message = err.Error()
					return nil
				}
				return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
			}
			if item == settingsItemKeybindings {
				settingsPage.showKeybindings = true
				return nil
			}
			if item == settingsItemDotfilesSetup {
				cmd, err := agent.CommandWithPrompt(dotfilesSetupPrompt)
				if err != nil {
					m.message = err.Error()
					return nil
				}
				return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
			}
			if item >= settingsItemToolStatusBase {
				idx := item - settingsItemToolStatusBase
				if idx < len(m.toolStatus) {
					openURL(m.toolStatus[idx].InstallURL)
					m.message = fmt.Sprintf("Opening %s", m.toolStatus[idx].InstallURL)
				}
				return nil
			}
			return settingsPage.enterSettingsEditing(m)
		}
	}

	return nil
}

// enterSettingsEditing activates text editing for the current setting.
func (settingsPage *settingsPage) enterSettingsEditing(m *model) tea.Cmd {
	if m.config == nil {
		m.config = state.DefaultConfig()
	}

	item := settingsPage.settingsCursorItem(m)
	var current string
	switch {
	case item == settingsItemDotfilesRepo:
		current = m.config.DotfilesSettings.RepoURL
		settingsPage.input.Placeholder = "https://github.com/user/dotfiles"
	case item == settingsItemDotfilesScript:
		current = m.config.DotfilesSettings.InstallScript
		settingsPage.input.Placeholder = "install.sh"
	case item == settingsItemCoderTemplate:
		current = m.config.CoderSettings.Template
		settingsPage.input.Placeholder = "template-name"
	case item >= settingsItemCoderParamBase && item < settingsItemCodespacesMachine:
		idx := item - settingsItemCoderParamBase
		if idx < len(m.config.CoderSettings.Parameters) {
			current = m.config.CoderSettings.Parameters[idx].Value
			param := m.config.CoderSettings.Parameters[idx]
			if param.DefaultValue != "" {
				settingsPage.input.Placeholder = param.DefaultValue
			} else {
				settingsPage.input.Placeholder = "value"
			}
		}
	case item == settingsItemCodespacesMachine:
		settingsPage.cycleCodespacesMachine(m, 1)
		return nil
	default:
		return nil
	}

	settingsPage.editing = true
	settingsPage.input.SetValue(current)
	settingsPage.input.Focus()
	settingsPage.input.CursorEnd()
	return settingsPage.input.Cursor.BlinkCmd()
}

// updateSettingsEditing handles input while editing a text field.
func (settingsPage *settingsPage) updateSettingsEditing(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			value := strings.TrimSpace(settingsPage.input.Value())
			if m.config == nil {
				m.config = state.DefaultConfig()
			}

			item := settingsPage.settingsCursorItem(m)
			var cmd tea.Cmd
			switch {
			case item == settingsItemDotfilesRepo:
				m.config.DotfilesSettings.RepoURL = value
			case item == settingsItemDotfilesScript:
				m.config.DotfilesSettings.InstallScript = value
			case item == settingsItemCoderTemplate:
				oldTemplate := m.config.CoderSettings.Template
				m.config.CoderSettings.Template = value
				if value != "" && value != oldTemplate {
					m.coderFetchingParams = true
					m.message = "Fetching template parameters..."
					cmd = fetchCoderParamsCmd(value)
				}
			case item >= settingsItemCoderParamBase && item < settingsItemCodespacesMachine:
				idx := item - settingsItemCoderParamBase
				if idx < len(m.config.CoderSettings.Parameters) {
					m.config.CoderSettings.Parameters[idx].Value = value
				}
			}

			if err := state.SaveConfig(m.config); err != nil {
				m.message = fmt.Sprintf("Failed to save settings: %v", err)
			} else if cmd == nil {
				m.message = "Saved"
			}
			settingsPage.editing = false
			settingsPage.input.Blur()
			return cmd

		case tea.KeyEsc:
			settingsPage.editing = false
			settingsPage.input.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	var cmd tea.Cmd
	settingsPage.input, cmd = settingsPage.input.Update(msg)
	return cmd
}

// ===========================================
// View
// ===========================================

// viewSettings renders the settings page.
func (settingsPage *settingsPage) viewSettings(m *model) string {
	var b strings.Builder

	b.WriteString(renderGradient(nameToBanner("Settings")))
	if m.updateAvailable != "" {
		b.WriteString("  " + updateStyle.Render(fmt.Sprintf("A new version: %s is available ⚡ ", m.updateAvailable)))
	}
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	config := m.config
	if config == nil {
		config = state.DefaultConfig()
	}

	box := listBox
	if m.width > 0 {
		box = box.Width(m.width - 2)
	}

	ruleWidth := 28
	if m.width > 0 {
		ruleWidth = max(1, m.width-2-box.GetHorizontalFrameSize())
	}

	var listContent strings.Builder
	currentItem := settingsPage.settingsCursorItem(m)

	// Record where each settings row lands on screen so mouse clicks
	// can resolve back to a cursor index. The box adds a top border
	// before its content, hence +1.
	clear(settingsPage.itemRowYs)
	clear(settingsPage.itemHeights)
	firstContentY := strings.Count(b.String(), "\n") + 1
	recordRow := func(item int, content string) {
		settingsPage.itemRowYs[item] = firstContentY + strings.Count(listContent.String(), "\n")
		settingsPage.itemHeights[item] = 1 + strings.Count(content, "\n")
		listContent.WriteString(content)
	}

	for _, section := range settingsSections {
		if !settingsPage.sectionVisible(m, section) {
			continue
		}

		listContent.WriteString(fleetExpandedStyle.Render(section.Title))
		listContent.WriteString("\n")
		listContent.WriteString(dimStyle.Render(strings.Repeat("─", ruleWidth)))
		listContent.WriteString("\n\n")

		switch section.Title {
		case "General":
			vimKeysValue := "[ off ]"
			if config.GeneralSettings.TmuxVimKeysEnabled() {
				vimKeysValue = "[ on ]"
			}
			recordRow(settingsItemTmuxVimKeys, settingsPage.renderSettingsRow(m, currentItem == settingsItemTmuxVimKeys, "Tmux vim keys", vimKeysValue))
			listContent.WriteString("\n")

			helpTextValue := "[ off ]"
			if config.GeneralSettings.ShowHelpTextEnabled() {
				helpTextValue = "[ on ]"
			}
			recordRow(settingsItemShowHelpText, settingsPage.renderSettingsRow(m, currentItem == settingsItemShowHelpText, "Show help text", helpTextValue))

			if m.updateAvailable != "" {
				listContent.WriteString("\n")
				updateValue := updateStyle.Render(m.updateAvailable+" available ⚡") + "  " + dimStyle.Render("press enter to update")
				recordRow(settingsItemUpdate, settingsPage.renderSettingsRow(m, currentItem == settingsItemUpdate, "Update", updateValue))
			}

		case "Dotfiles":
			repoValue := config.DotfilesSettings.RepoURL
			if repoValue == "" && !(settingsPage.editing && currentItem == settingsItemDotfilesRepo) {
				repoValue = dimStyle.Render("(not set)")
			}
			recordRow(settingsItemDotfilesRepo, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesRepo, "Repository URL", repoValue))
			listContent.WriteString("\n")

			scriptValue := config.DotfilesSettings.InstallScript
			if scriptValue == "" && !(settingsPage.editing && currentItem == settingsItemDotfilesScript) {
				scriptValue = dimStyle.Render("(not set)")
			}
			recordRow(settingsItemDotfilesScript, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesScript, "Install script", scriptValue))
			listContent.WriteString("\n")

			autoInstallValue := "[ off ]"
			if config.DotfilesSettings.AutoInstall {
				autoInstallValue = "[ on ]"
			}
			recordRow(settingsItemDotfilesAutoInstall, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesAutoInstall, "Auto install", autoInstallValue))
			listContent.WriteString("\n")

			agentName, _, agentErr := agent.FindAgent()
			var setupValue string
			if agentErr != nil {
				setupValue = statusCreatingStyle.Render("no agent found") + "  " + dimStyle.Render("install claude, codex, gemini, or copilot")
			} else {
				setupValue = statusRunningStyle.Render(agentName) + "  " + dimStyle.Render("press enter to get help setting up dotfiles")
			}
			recordRow(settingsItemDotfilesSetup, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesSetup, "Help me set this up", setupValue))

		case "Coder":
			templateValue := config.CoderSettings.Template
			if templateValue == "" && !(settingsPage.editing && currentItem == settingsItemCoderTemplate) {
				templateValue = dimStyle.Render("(not set)")
			}
			if m.coderFetchingParams {
				templateValue += "  " + m.spinner.View() + " fetching..."
			}
			recordRow(settingsItemCoderTemplate, settingsPage.renderSettingsRow(m, currentItem == settingsItemCoderTemplate, "Template", templateValue))
			listContent.WriteString("\n")

			presetValue := config.CoderSettings.Preset
			if presetValue == "" {
				presetValue = dimStyle.Render("(none)")
			} else {
				presetValue = fmt.Sprintf("[ %s ]", presetValue)
			}
			recordRow(settingsItemCoderPreset, settingsPage.renderSettingsRow(m, currentItem == settingsItemCoderPreset, "Preset", presetValue))

			for i, param := range config.CoderSettings.Parameters {
				listContent.WriteString("\n")
				paramItem := settingsItemCoderParamBase + i
				value := param.Value
				if value == "" && !(settingsPage.editing && currentItem == paramItem) {
					if param.DefaultValue != "" {
						value = dimStyle.Render(param.DefaultValue + " (default)")
					} else {
						value = dimStyle.Render("(not set)")
					}
				}
				label := param.Name
				if param.DisplayName != "" {
					label = param.DisplayName
				}
				recordRow(paramItem, settingsPage.renderSettingsRow(m, currentItem == paramItem, label, value))
			}

		case "Codespaces":
			var machineValue string
			if config.CodespacesSettings.Machine == "" {
				if m.codespaceFetchingMachines {
					machineValue = m.spinner.View() + " fetching..."
				} else {
					machineValue = dimStyle.Render("(none)")
				}
			} else {
				machineValue = fmt.Sprintf("[ %s ]", config.CodespacesSettings.Machine)
				if label := settingsPage.codespacesMachineLabel(m); label != config.CodespacesSettings.Machine {
					machineValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render(label)
				}
			}
			recordRow(settingsItemCodespacesMachine, settingsPage.renderSettingsRow(m, currentItem == settingsItemCodespacesMachine, "Machine", machineValue))

		case "Browser":
			multipleValue := "[ off ]"
			if config.BrowserSettings.MultipleBrowsersPerFleetEnabled() {
				multipleValue = "[ on ]"
			}
			recordRow(settingsItemBrowserMultiple, settingsPage.renderSettingsRow(m, currentItem == settingsItemBrowserMultiple, "Enable Multiple Browsers Per Fleet", multipleValue))

			if !config.BrowserSettings.MultipleBrowsersPerFleetEnabled() {
				listContent.WriteString("\n")
				autoSwitchValue := "[ off ]"
				if config.BrowserSettings.AutoSwitchEnabled() {
					autoSwitchValue = "[ on ]"
				}
				// Append a dim sub-line under the value so the
				// setting carries its own one-line description.
				// The 21-space indent matches cursor (2) + label
				// (%-18s) + value-separator (1).
				autoSwitchValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render("Do not prompt when switching the browser to another instance")
				recordRow(settingsItemBrowserAutoSwitch, settingsPage.renderSettingsRow(m, currentItem == settingsItemBrowserAutoSwitch, "Auto Switch", autoSwitchValue))
			}

		case "Tool Status":
			for i, tool := range m.toolStatus {
				if i > 0 {
					listContent.WriteString("\n")
				}
				var badge string
				if tool.Found {
					badge = statusRunningStyle.Render("installed")
				} else {
					badge = statusCreatingStyle.Render("not found")
				}
				value := badge + "  " + dimStyle.Render(tool.Description)
				itemID := settingsItemToolStatusBase + i
				recordRow(itemID, settingsPage.renderSettingsRow(m, currentItem == itemID, tool.Name, value))
			}

		case "Help":
			agentName, _, agentErr := doctor.FindAgent()
			var value string
			if agentErr != nil {
				value = statusCreatingStyle.Render("no agent found") + "  " + dimStyle.Render("install claude, codex, gemini, or copilot")
			} else {
				value = statusRunningStyle.Render(agentName) + "  " + dimStyle.Render("press enter to diagnose your setup")
			}
			recordRow(settingsItemDoctor, settingsPage.renderSettingsRow(m, currentItem == settingsItemDoctor, "Run Doctor", value))
			listContent.WriteString("\n")
			recordRow(settingsItemKeybindings, settingsPage.renderSettingsRow(m, currentItem == settingsItemKeybindings, "Keybindings", dimStyle.Render("press enter to view all keybindings")))
		}

		listContent.WriteString("\n\n")
	}

	b.WriteString(box.Render(strings.TrimRight(listContent.String(), "\n")))
	b.WriteString("\n")

	if settingsPage.showKeybindings {
		b.WriteString("\n")
		b.WriteString(keybindingsDialogBox.Render(settingsPage.renderKeybindingsDialog()))
		b.WriteString("\n")
	}

	if currentItem >= settingsItemCoderParamBase && currentItem < settingsItemToolStatusBase {
		b.WriteString(dimStyle.Render("  Variables: ${GIT_URL} = fleet repo URL, ${GIT_BRANCH} = git branch (blank = default), ${INSTANCE_NAME} = workspace name"))
		b.WriteString("\n")
	}

	if m.message != "" {
		b.WriteString(messageStyle.Render(m.message))
		b.WriteString("\n")
	}

	if settingsPage.editing {
		b.WriteString(renderHelp(m.width, []string{
			"enter: save", "esc: cancel",
		}))
	} else {
		b.WriteString(renderHelp(m.width, []string{
			"j/k: navigate", "left/right: cycle", "enter: edit", "esc: back", "ctrl+c: quit",
		}))
	}

	return b.String()
}

// renderSettingsRow renders a single settings row with optional cursor
// and editing state.
func (settingsPage *settingsPage) renderSettingsRow(m *model, active bool, label string, value string) string {
	cursor := "  "
	if active {
		cursor = cursorStyle.Render("> ")
	}

	formattedLabel := fmt.Sprintf("%-18s", label)

	if settingsPage.editing && active {
		value = settingsPage.input.View()
	}

	if active {
		return fmt.Sprintf("%s%s %s", cursor, selectedStyle.Render(formattedLabel), value)
	}
	return fmt.Sprintf("%s%s %s", cursor, formattedLabel, value)
}

// ===========================================
// URL Helper
// ===========================================

// openURL opens the given URL in the user's default browser.
func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

package tui

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Fleet Page
// ===========================================

var resolveWorkspaceBranch = gitutil.BranchName

// fleetPage holds fleet-list-specific state.
type fleetPage struct {
	rows      []row
	cursor    int
	collapsed map[string]bool

	// focusedFleet, when non-empty, names the single fleet shown in focus
	// mode. All other fleets are hidden from the list, the help bar is hidden,
	// the "settings" row becomes "[ leave focus ]", and q/esc leave focus
	// instead of quitting (focus behaves like a dialog).
	focusedFleet string

	// mode selects which dialog/interaction is active; only one is open at a
	// time, so the dialogs below reuse the shared dlg scratch and never
	// coexist. Per-dialog state is grouped into its own sub-struct (see the
	// type definitions following fleetPage) rather than flattened here.
	mode viewMode

	dlg        dialogContext      // shared scratch reused by every dialog
	addInst    addInstanceState   // add/edit-instance form
	editFleet  editFleetState     // edit-fleet settings dialog
	addFleet   addFleetState      // new-fleet URL → inspect flow
	newSession createSessionState // new-session preset templating
	browserDlg browserDialogState // switch-browser dialog
	choosePR   choosePRState      // multi-PR chooser (open which PR?)
	lpFlow     *layoutPresetFlow  // open preset creation/edit flow (nil unless mode == viewLayoutPreset)

	// rightSelected is true when the cursor row's right-hand element is
	// horizontally selected — →/l selects it (drawn pink), ←/h deselects, enter
	// activates it. Two row kinds carry such an element: an instance's PR-status
	// auto tag (enter opens the PR) and a fleet header's ⟳/☰
	// mode toggle (enter flips automation mode). Cleared on any vertical cursor
	// move. See currentRowHasRightElement / activateRightSelection.
	rightSelected bool

	textInput          textinput.Model
	branchInput        textinput.Model
	homedirInput       textinput.Model
	customMountInput   textinput.Model
	coderWsNameInput   textinput.Model // edit-fleet Coder section: workspace-name override
	coderTemplateInput textinput.Model // edit-fleet Coder section: template
	coderParamInput    textinput.Model // edit-fleet Coder section: shared input for the focused parameter row

	pfCursor int

	split       splitState // open split pane + session-group restore bookkeeping
	savedGroups map[string]savedGroup

	// listRowY is the terminal Y (0-indexed) where rows[0] is rendered,
	// recorded during View() so mouse clicks can be mapped back to a
	// row index. -1 means "not yet rendered" or "no clickable rows".
	listRowY int

	armadaSel armadaSelectState // Armada selector widget (border label + dropdown)

	// automationMode[fleet] is true when that fleet renders its automation view
	// (collapsible triggers + agents groups) instead of its instance list
	// (issue #188). Toggled with 'm'; persists across route changes like the
	// rest of fleetPage.
	automationMode map[string]bool
	// autoCollapsed holds the collapse state of the per-fleet automation groups,
	// keyed "trig:<fleet>" / "agent:<fleet>"; an absent key == expanded.
	autoCollapsed map[string]bool
	triggerDlg    automationTriggerState // add/edit-trigger dialog
	agentDlg      automationAgentState   // add/edit-agent dialog
	autoDel       autoDeleteState        // pending trigger/agent delete (viewConfirmDeleteAutomation)
}

// newFleetPage creates a new fleet page with default state.
func newFleetPage() *fleetPage {
	nameInput := textinput.New()
	nameInput.Placeholder = "instance-name"
	nameInput.CharLimit = 64

	branchInput := textinput.New()
	branchInput.Placeholder = "default branch"
	branchInput.CharLimit = 128

	homedirInput := textinput.New()
	homedirInput.Placeholder = "/home/vscode"
	homedirInput.CharLimit = 256

	customMountInput := textinput.New()
	customMountInput.Placeholder = "/opt/data"
	customMountInput.CharLimit = 256

	coderWsNameInput := textinput.New()
	coderWsNameInput.Placeholder = "workspace-name"
	coderWsNameInput.CharLimit = 64

	coderTemplateInput := textinput.New()
	coderTemplateInput.Placeholder = "template-name"
	coderTemplateInput.CharLimit = 128

	coderParamInput := textinput.New()
	coderParamInput.Placeholder = "value"
	coderParamInput.CharLimit = 256

	return &fleetPage{
		collapsed:          make(map[string]bool),
		savedGroups:        make(map[string]savedGroup),
		textInput:          nameInput,
		branchInput:        branchInput,
		homedirInput:       homedirInput,
		customMountInput:   customMountInput,
		coderWsNameInput:   coderWsNameInput,
		coderTemplateInput: coderTemplateInput,
		coderParamInput:    coderParamInput,
		listRowY:           -1,
		armadaSel:          armadaSelectState{y: -1},
		automationMode:     make(map[string]bool),
		autoCollapsed:      make(map[string]bool),
		triggerDlg:         automationTriggerState{input: newAutomationInput()},
		agentDlg:           automationAgentState{input: newAutomationInput()},
	}
}

// Init is called when the fleet page becomes active.
func (fleetPage *fleetPage) Init(m *model) tea.Cmd {
	fleetPage.buildRows(m)
	return nil
}

// Update dispatches messages to the appropriate handler based on the
// current view mode.
func (fleetPage *fleetPage) Update(m *model, msg tea.Msg) tea.Cmd {
	// Fleet-specific async messages that need row rebuilds. (Session-list
	// refresh + the split-layout snapshot / external-kill detection ride the
	// runtimeChangedMsg handler in the model-level Update now — the server pushes
	// session changes on the runtime stream, replacing the old client poll.)
	switch msg.(type) {
	case editorFinishedMsg:
		return fleetPage.applyEditorResult(m, msg.(editorFinishedMsg))

	case operationDoneMsg:
		fleetPage.buildRows(m)
		return nil

	case instanceCreateErrMsg:
		fleetPage.buildRows(m)
		return nil

	case browserProxyMsg:
		proxyMsg := msg.(browserProxyMsg)
		if proxyMsg.err != nil {
			m.message = fmt.Sprintf("Browser proxy error: %v", proxyMsg.err)
		} else {
			m.message = fmt.Sprintf("Browser opened (proxy on localhost:%d)", proxyMsg.localPort)
			// Record which instance the browser bound to this data dir now
			// serves, so a later control-socket open for a different instance
			// switches the browser over rather than new-tabbing into the wrong
			// proxy. Recompute the data dir the same way the open path did.
			if fleetName, instanceName, ok := splitInstanceKey(proxyMsg.instanceKey); ok {
				dataDir := browserDataDir(fleetName, instanceName, multipleBrowsersPerFleet(m))
				if m.activeBrowser == nil {
					m.activeBrowser = make(map[string]string)
				}
				m.activeBrowser[dataDir] = proxyMsg.instanceKey
			}
		}
		// Tear down the switching-spinner dialog if it was active.
		if fleetPage.browserDlg.switching {
			fleetPage.browserDlg.switching = false
			fleetPage.mode = viewNormal
		}
		return nil

	case externalURLOpenedMsg:
		if u := msg.(externalURLOpenedMsg); u.err != nil {
			m.message = fmt.Sprintf("Could not open browser: %v", u.err)
		}
		return nil

	case homedirDetectedMsg:
		return fleetPage.handleHomedirDetected(m, msg.(homedirDetectedMsg))

	case coderParamsFetchedMsg:
		return fleetPage.handleCoderParamsFetched(m, msg.(coderParamsFetchedMsg))

	case deleteCacheDoneMsg:
		return fleetPage.handleDeleteCacheDone(m, msg.(deleteCacheDoneMsg))

	case devcontainerInspectedMsg:
		return fleetPage.handleDevcontainerInspected(m, msg.(devcontainerInspectedMsg))
	}

	// Mode-specific dispatch
	switch fleetPage.mode {
	case viewConfirmDelete:
		return fleetPage.updateConfirmDelete(m, msg)
	case viewConfirmRebuild:
		return fleetPage.updateConfirmRebuild(m, msg)
	case viewConfirmDeleteFleetWarn:
		return fleetPage.updateConfirmDeleteFleetWarn(m, msg)
	case viewAddInstance:
		return fleetPage.updateAddInstance(m, msg)
	case viewAddFleet:
		return fleetPage.updateAddFleet(m, msg)
	case viewAddFleetName:
		return fleetPage.updateAddFleetName(m, msg)
	case viewAddFleetInspecting:
		return fleetPage.updateAddFleetInspecting(m, msg)
	case viewAddFleetNoDevcontainer:
		return fleetPage.updateAddFleetNoDevcontainer(m, msg)
	case viewEditFleet:
		return fleetPage.updateEditFleet(m, msg)
	case viewTagInstance:
		return fleetPage.updateTagInstance(m, msg)
	case viewPortForward:
		return fleetPage.updatePortForward(m, msg)
	case viewCodespacesAuth:
		return fleetPage.updateCodespacesAuth(m, msg)
	case viewCodespacesLimit:
		return fleetPage.updateCodespacesLimit(m, msg)
	case viewCodespacesMachine:
		return fleetPage.updateCodespacesMachine(m, msg)
	case viewConfirmDeleteSession:
		return fleetPage.updateConfirmDeleteSession(m, msg)
	case viewConfirmBrowserSwitch:
		return fleetPage.updateConfirmBrowserSwitch(m, msg)
	case viewChooseBrowserLaunch:
		return fleetPage.updateChooseBrowserLaunch(m, msg)
	case viewChoosePR:
		return fleetPage.updateChoosePR(m, msg)
	case viewArmadaSelect:
		return fleetPage.updateArmadaSelect(m, msg)
	case viewCreateSession:
		return fleetPage.updateCreateSession(m, msg)
	case viewAutomationTrigger:
		return fleetPage.updateAutomationTrigger(m, msg)
	case viewAutomationAgent:
		return fleetPage.updateAutomationAgent(m, msg)
	case viewConfirmDeleteAutomation:
		return fleetPage.updateConfirmDeleteAutomation(m, msg)
	case viewLayoutPreset:
		return fleetPage.updateLayoutPreset(m, msg)
	case viewRenameSession:
		return fleetPage.updateRenameSession(m, msg)
	case viewCloneInstance:
		return fleetPage.updateCloneInstance(m, msg)
	default:
		return fleetPage.updateNormal(m, msg)
	}
}

// View renders the fleet list page.
func (fleetPage *fleetPage) View(m *model) string {
	return fleetPage.viewFleetList(m)
}

// ===========================================
// Row Management
// ===========================================

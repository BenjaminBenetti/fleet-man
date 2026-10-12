package tui

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Fleet Page
// ===========================================

// instanceBranch is the branch shown on an instance row. The daemon reads it
// from the workspace (InstanceRuntime.current_branch, pushed over Watch); the
// TUI never reads a checkout itself, since it may run on another machine than
// the daemon. Without a live read (no push yet, or a workspace that can't be
// read while the instance is stopped) it falls back to the last branch seen,
// then the requested one.
func instanceBranch(rt *fleetgrpc.InstanceRuntime, instance *fleet.Instance) string {
	var live *string
	if rt != nil {
		live = rt.CurrentBranch
	}
	return fleet.DisplayBranch(live, instance.LastBranch, instance.Branch)
}

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
	// saveWarnings is what the daemon warned about the fleet settings it
	// saved last (noteSaveWarnings).
	saveWarnings []string
	// removingFleets are the empty fleets whose removal job runs
	// (removeEmptyFleet): kept off the list until it ends, though the
	// daemon lists them until then.
	removingFleets map[string]bool
	// kubeRecovery is the Kubernetes recovery dialog (mode == viewKubeRecovery).
	kubeRecovery kubeRecoveryState
	// vscodeDlg is the VS Code Remote-SSH consent dialog (mode == viewEditorConsent).
	vscodeDlg vscodeConsentState

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
	mountHostInput     textinput.Model // edit-fleet Custom mounts: a mount's host folder
	coderWsNameInput   textinput.Model // edit-fleet Coder section: workspace-name override
	coderTemplateInput textinput.Model // edit-fleet Coder section: template
	coderParamInput    textinput.Model // edit-fleet Coder section: shared input for the focused parameter row

	pfCursor int
	// pfHalfClose is the port-forward dialog's half-close toggle: the next
	// forward it adds keeps a true half-close (`fleet port-forward add
	// --half-close`).
	pfHalfClose bool

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
	// pendingLogin is a Log in action waiting for the instance it created to
	// run (automation_login.go); nil when none waits.
	pendingLogin *pendingLogin
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

	mountHostInput := textinput.New()
	mountHostInput.Placeholder = "/data/big (empty: the .mnt folder)"
	mountHostInput.CharLimit = 512

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
		mountHostInput:     mountHostInput,
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
		if done := msg.(operationDoneMsg); done.instance == "" {
			// A fleet's removal ended: what the daemon still lists shows.
			delete(fleetPage.removingFleets, done.fleet)
		}
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

	case vscodeResolvedMsg:
		return fleetPage.handleVSCodeResolved(m, msg.(vscodeResolvedMsg))

	case vscodeOpenedMsg:
		return fleetPage.handleVSCodeOpened(m, msg.(vscodeOpenedMsg))

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
	case cacheUsageMsg:
		return fleetPage.handleCacheUsage(msg.(cacheUsageMsg))

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
	case viewKubeRecovery:
		return fleetPage.updateKubeRecovery(m, msg)
	case viewEditorConsent:
		return fleetPage.updateEditorConsent(m, msg)
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

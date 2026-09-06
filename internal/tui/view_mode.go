package tui

// ===========================================
// View Mode
// ===========================================

// viewMode identifies which dialog or interaction mode the fleet page
// is currently in. The default (zero) value, viewNormal, means the
// normal fleet-list keyboard navigation is active.
type viewMode int

const (
	viewNormal viewMode = iota
	viewConfirmDelete
	viewConfirmRebuild
	viewConfirmDeleteFleetWarn
	viewAddInstance
	viewCloneInstance
	viewAddFleet
	viewAddFleetName
	viewAddFleetInspecting
	viewAddFleetNoDevcontainer
	viewEditFleet
	viewTagInstance
	viewPortForward
	viewCodespacesAuth
	viewCodespacesLimit
	viewCodespacesMachine
	viewCreateSession
	viewLayoutPreset
	viewRenameSession
	viewConfirmDeleteSession
	viewConfirmBrowserSwitch
	viewChooseBrowserLaunch
	viewArmadaSelect
	viewChoosePR
	viewAutomationTrigger
	viewAutomationAgent
	viewConfirmDeleteAutomation
)

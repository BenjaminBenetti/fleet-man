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
	viewConfirmDeleteFleetWarn
	viewAddInstance
	viewAddFleet
	viewTagInstance
	viewPortForward
	viewCodespacesAuth
	viewCodespacesLimit
	viewCodespacesMachine
	viewCreateSession
	viewRenameSession
	viewConfirmDeleteSession
)

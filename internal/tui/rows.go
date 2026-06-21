package tui

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// ===========================================
// Row Types (shared, used by fleet page)
// ===========================================

// rowKind identifies the variant of a navigable row in the TUI list.
type rowKind int

const (
	rowFleetHeader rowKind = iota
	rowInstance
	rowInstanceTag
	rowSession
	rowNewSession
	rowSettings
	rowLeaveFocus
	// Automation-view rows (issue #188). A fleet in automation mode renders two
	// collapsible groups — triggers and agents — in place of its instances.
	rowAutomationTriggers // triggers group header
	rowAutomationAgents   // agents group header
	rowTrigger            // a trigger child (autoIdx indexes Settings.Triggers)
	rowAgent              // an agent child (autoIdx indexes Settings.Agents)
	rowNewTrigger         // "+ add trigger" action row
	rowNewAgent           // "+ add agent" action row
)

// row represents a single navigable row in the TUI.
type row struct {
	kind        rowKind
	fleetName   string
	instance    *fleet.Instance
	sessionName string // set when kind == rowSession or rowNewSession
	groupID     string // set for grouped session rows
	groupSize   int    // number of sessions in the group (for display)
	// autoIdx indexes the fleet's Settings.Triggers (rowTrigger) or
	// Settings.Agents (rowAgent) — the automation item this row renders/edits.
	autoIdx int
	// toggleX0/toggleX1 record the absolute terminal column span of the
	// [automations]/[instances] mode-toggle button on a rowFleetHeader (set
	// during View). A left click landing in [toggleX0, toggleX1) toggles the
	// fleet's automation mode instead of collapsing it. toggleX1 == toggleX0
	// means "no button here".
	toggleX0 int
	toggleX1 int
	// prStatusInline marks the first child row of an expanded instance that has
	// no user tag, telling the renderer to show the instance's PR-status auto tag
	// at the status column on this row (a "second status line"). See buildRows.
	prStatusInline bool
}

// selectable reports whether the cursor may rest on this row. Instance
// tag rows are display-only; navigation and mouse clicks skip them.
func (r row) selectable() bool {
	return r.kind != rowInstanceTag
}

// lastSession tracks the most recently used session for an instance,
// allowing reconnection on subsequent enter presses instead of always
// creating a new session.
type lastSession struct {
	sessionName string
	groupID     string
}

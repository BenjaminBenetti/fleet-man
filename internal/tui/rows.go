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
	rowSession
	rowNewSession
	rowSettings
)

// row represents a single navigable row in the TUI.
type row struct {
	kind        rowKind
	fleetName   string
	instance    *fleet.Instance
	sessionName string // set when kind == rowSession or rowNewSession
	groupID     string // set for grouped session rows
	groupSize   int    // number of sessions in the group (for display)
}

// lastSession tracks the most recently used session for an instance,
// allowing reconnection on subsequent enter presses instead of always
// creating a new session.
type lastSession struct {
	sessionName string
	groupID     string
}

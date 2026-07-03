package tui

import (
	"sort"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// buildRows rebuilds the navigable row list from the current state.
func (fleetPage *fleetPage) buildRows(m *model) {
	// Remember whether the cursor sat on the trailing action row (settings or
	// its focus-mode replacement) so it stays pinned there across the rebuild.
	wasOnActionRow := false
	if r := fleetPage.currentRow(); r != nil && (r.kind == rowSettings || r.kind == rowLeaveFocus) {
		wasOnActionRow = true
	}

	fleetPage.rows = nil

	// A focused fleet that no longer exists (deleted while focused) drops focus
	// so the list doesn't render empty.
	if fleetPage.focusedFleet != "" {
		if _, ok := m.st.Fleets[fleetPage.focusedFleet]; !ok {
			fleetPage.focusedFleet = ""
		}
	}

	names := sortedFleetNames(m.st.Fleets)

	for _, name := range names {
		// Focus mode renders only the focused fleet; everything else is hidden.
		if fleetPage.focusedFleet != "" && name != fleetPage.focusedFleet {
			continue
		}
		f := m.st.Fleets[name]
		fleetPage.rows = append(fleetPage.rows, row{kind: rowFleetHeader, fleetName: name})
		if !fleetPage.collapsed[name] && fleetPage.automationMode[name] {
			fleetPage.appendAutomationRows(name, f)
			continue
		}
		if !fleetPage.collapsed[name] {
			for _, instance := range f.Instances {
				fleetPage.rows = append(fleetPage.rows, row{kind: rowInstance, fleetName: name, instance: instance})
				ref := InstanceRef{Fleet: name, Instance: instance.Name}
				if m.sessionStore.IsExpanded(ref) {
					// A user-set tag gets its own line under the instance name. The
					// PR-status auto tag instead rides the first child row's status
					// column (handled after the child rows are built, below), so the
					// two never share a line.
					if instance.Tag != "" {
						fleetPage.rows = append(fleetPage.rows, row{kind: rowInstanceTag, fleetName: name, instance: instance})
					}
					childStart := len(fleetPage.rows)
					liveGroups := make(map[string]bool)
					for _, g := range m.sessionStore.Groups(ref) {
						liveGroups[g.GroupID] = true
						rootName := g.Sessions[0].Name
						// Tab badge only for single-session rows: a
						// multi-pane group already carries the panes
						// suffix, and its per-pane tab counts differ.
						tabCount := 0
						if len(g.Sessions) == 1 {
							tabCount = g.Sessions[0].Windows
						}
						fleetPage.rows = append(fleetPage.rows, row{
							kind:        rowSession,
							fleetName:   name,
							instance:    instance,
							sessionName: rootName,
							groupID:     g.GroupID,
							groupSize:   len(g.Sessions),
							tabCount:    tabCount,
						})
					}
					fleetPage.appendSavedGroupRows(name, instance, liveGroups)
					fleetPage.rows = append(fleetPage.rows, row{
						kind:      rowNewSession,
						fleetName: name,
						instance:  instance,
					})
					// Attach the PR status to the first child row (first session, or
					// "+ new session" when there are none) when no user tag occupies
					// the slot — it renders as a second status line under the status.
					if instance.Tag == "" && m.instanceAutoTag(name, instance.Name, false) != "" {
						fleetPage.rows[childStart].prStatusInline = true
					}
				}
			}
		}
	}
	if fleetPage.focusedFleet != "" {
		fleetPage.rows = append(fleetPage.rows, row{kind: rowLeaveFocus})
	} else {
		fleetPage.rows = append(fleetPage.rows, row{kind: rowSettings})
	}
	if wasOnActionRow {
		fleetPage.cursor = len(fleetPage.rows) - 1
	}
	if fleetPage.cursor >= len(fleetPage.rows) {
		fleetPage.cursor = max(0, len(fleetPage.rows)-1)
	}
	// A rebuild can shift rows so the cursor lands on a display-only row
	// (e.g. a tag line inserted above a session row); nudge it forward.
	if r := fleetPage.currentRow(); r != nil && !r.selectable() {
		fleetPage.moveCursor(1)
	}

	// Drop a stale right-hand selection if the cursor no longer sits on a row with
	// a selectable right-hand element (e.g. a PR closed, or a rebuild moved the
	// cursor off the row carrying it).
	if fleetPage.rightSelected && !fleetPage.currentRowHasRightElement(m) {
		fleetPage.rightSelected = false
	}
}

// enterFocus switches the list into focus mode for the named fleet, hiding all
// other fleets and parking the cursor on the focused fleet's header.
func (fleetPage *fleetPage) enterFocus(m *model, name string) {
	if name == "" {
		return
	}
	if _, ok := m.st.Fleets[name]; !ok {
		return
	}
	fleetPage.focusedFleet = name
	fleetPage.armadaSel.focused = false
	fleetPage.buildRows(m)
	fleetPage.cursorToFleetHeader(name)
	m.message = ""
}

// leaveFocus exits focus mode and restores the cursor to the fleet that was
// focused so the user keeps their place in the full list. It also drops any
// Armada-selector focus so the row cursor is visible again afterwards.
func (fleetPage *fleetPage) leaveFocus(m *model) {
	if fleetPage.focusedFleet == "" {
		return
	}
	name := fleetPage.focusedFleet
	fleetPage.focusedFleet = ""
	fleetPage.armadaSel.focused = false
	fleetPage.buildRows(m)
	fleetPage.cursorToFleetHeader(name)
	m.message = ""
}

// cursorToFleetHeader points the cursor at the named fleet's header row, if it
// is present in the current row list.
func (fleetPage *fleetPage) cursorToFleetHeader(name string) {
	for i, r := range fleetPage.rows {
		if r.kind == rowFleetHeader && r.fleetName == name {
			fleetPage.cursor = i
			return
		}
	}
}

func (fleetPage *fleetPage) appendSavedGroupRows(fleetName string, instance *fleet.Instance, liveGroups map[string]bool) {
	sanitized := SanitizeSessionName(instance.Name)
	for _, group := range fleetPage.savedGroupsForInstance(instance.Name) {
		if liveGroups[group.GroupID] {
			continue
		}
		sessions := savedGroupSessionNames(group, sanitized)
		fleetPage.rows = append(fleetPage.rows, row{
			kind:        rowSession,
			fleetName:   fleetName,
			instance:    instance,
			sessionName: sessions[0],
			groupID:     group.GroupID,
			groupSize:   savedGroupPaneCount(group),
		})
	}
}

// appendAutomationRows emits a fleet's automation view: a collapsible triggers
// group and a collapsible agents group, each with its items and a trailing
// "+ add" action row. Mirrors the instance/session row structure so navigation,
// cursor nudging, and rendering reuse the same machinery.
func (fleetPage *fleetPage) appendAutomationRows(name string, f *fleet.Fleet) {
	fleetPage.rows = append(fleetPage.rows, row{kind: rowAutomationTriggers, fleetName: name})
	if !fleetPage.triggersCollapsed(name) {
		for i := range f.Settings.Triggers {
			fleetPage.rows = append(fleetPage.rows, row{kind: rowTrigger, fleetName: name, autoIdx: i})
		}
		fleetPage.rows = append(fleetPage.rows, row{kind: rowNewTrigger, fleetName: name})
	}
	fleetPage.rows = append(fleetPage.rows, row{kind: rowAutomationAgents, fleetName: name})
	if !fleetPage.agentsCollapsed(name) {
		for i := range f.Settings.Agents {
			fleetPage.rows = append(fleetPage.rows, row{kind: rowAgent, fleetName: name, autoIdx: i})
		}
		fleetPage.rows = append(fleetPage.rows, row{kind: rowNewAgent, fleetName: name})
	}
}

func (fleetPage *fleetPage) triggersCollapsed(fleet string) bool {
	return fleetPage.autoCollapsed["trig:"+fleet]
}

func (fleetPage *fleetPage) agentsCollapsed(fleet string) bool {
	return fleetPage.autoCollapsed["agent:"+fleet]
}

// toggleAutomationMode flips the named fleet between its instance view and its
// automation view, parking the cursor on the fleet header so the toggle target
// stays in sight.
func (fleetPage *fleetPage) toggleAutomationMode(m *model, name string) {
	if name == "" {
		return
	}
	fleetPage.automationMode[name] = !fleetPage.automationMode[name]
	fleetPage.rightSelected = false
	fleetPage.buildRows(m)
	fleetPage.cursorToFleetHeader(name)
}

func (fleetPage *fleetPage) savedGroupsForInstance(instanceName string) []savedGroup {
	groups := make([]savedGroup, 0, len(fleetPage.savedGroups))
	for _, group := range fleetPage.savedGroups {
		if group.InstanceName == instanceName {
			groups = append(groups, group)
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].GroupID < groups[j].GroupID
	})
	return groups
}

// currentRow returns a pointer to the row at the cursor position.
func (fleetPage *fleetPage) currentRow() *row {
	if fleetPage.cursor < 0 || fleetPage.cursor >= len(fleetPage.rows) {
		return nil
	}
	return &fleetPage.rows[fleetPage.cursor]
}

// currentRowHasRightElement reports whether the cursor row carries a right-hand
// element that →/l can select and enter can activate: a fleet header's
// ⟳/☰ mode toggle, or an expanded instance's inline PR
// status.
func (fleetPage *fleetPage) currentRowHasRightElement(m *model) bool {
	r := fleetPage.currentRow()
	if r == nil {
		return false
	}
	return r.kind == rowFleetHeader || len(m.rowInlinePRRefs(*r)) > 0
}

// firstSelectable returns the index of the first selectable row (-1 if none).
func (fleetPage *fleetPage) firstSelectable() int {
	for i, r := range fleetPage.rows {
		if r.selectable() {
			return i
		}
	}
	return -1
}

// lastSelectable returns the index of the last selectable row (-1 if none).
func (fleetPage *fleetPage) lastSelectable() int {
	for i := len(fleetPage.rows) - 1; i >= 0; i-- {
		if fleetPage.rows[i].selectable() {
			return i
		}
	}
	return -1
}

// moveCursor moves the cursor by delta rows, wrapping around and
// skipping rows the cursor may not rest on (e.g. instance tag lines).
func (fleetPage *fleetPage) moveCursor(delta int) {
	n := len(fleetPage.rows)
	if n == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
		delta = -delta
	}
	for ; delta > 0; delta-- {
		for range n {
			fleetPage.cursor = (fleetPage.cursor + step + n) % n
			if fleetPage.rows[fleetPage.cursor].selectable() {
				break
			}
		}
	}
}

// moveCursorToInstance moves the cursor to the next (delta > 0) or previous
// (delta < 0) instance row, wrapping around. If the row list contains no
// instance rows, the cursor is left unchanged.
func (fleetPage *fleetPage) moveCursorToInstance(delta int) {
	n := len(fleetPage.rows)
	if n == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	for range n {
		fleetPage.cursor = (fleetPage.cursor + step + n) % n
		if fleetPage.rows[fleetPage.cursor].kind == rowInstance {
			return
		}
	}
}

// currentFleetName returns the fleet name for the row at the cursor.
func (fleetPage *fleetPage) currentFleetName() string {
	r := fleetPage.currentRow()
	if r == nil || r.kind == rowSettings || r.kind == rowLeaveFocus {
		return ""
	}
	return r.fleetName
}

// selectedInstance returns the fleet and instance when the cursor is
// on an instance row.
func (fleetPage *fleetPage) selectedInstance(m *model) (*fleet.Fleet, *fleet.Instance) {
	r := fleetPage.currentRow()
	if r == nil || r.kind != rowInstance || r.instance == nil {
		return nil, nil
	}
	f := m.st.Fleets[r.fleetName]
	return f, r.instance
}

// selectedSession returns the fleet, instance, and session name when
// the cursor is on a session row.
func (fleetPage *fleetPage) selectedSession(m *model) (*fleet.Fleet, *fleet.Instance, string) {
	r := fleetPage.currentRow()
	if r == nil || r.kind != rowSession {
		return nil, nil, ""
	}
	f := m.st.Fleets[r.fleetName]
	return f, r.instance, r.sessionName
}

// ===========================================
// Normal Mode Update
// ===========================================

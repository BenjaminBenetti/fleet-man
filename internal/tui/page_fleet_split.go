package tui

import (
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// splitState holds the open split pane plus the session-group restore
// bookkeeping. ref qualifies activeGroup so two groups sharing an ID across
// instances cannot alias.
type splitState struct {
	paneID     string
	ref        InstanceRef // (fleet, instance) of the open split pane; zero when none
	session    string
	openedAt   time.Time // when the current split pane was opened; for "session closed" duration
	viaRestore bool      // true when the split was opened via restoreGroupCmd (fleet shell logs its own open/close, so the TUI must not duplicate it)

	activeGroup      ActiveGroup
	pendingGroup     ActiveGroup
	debounceSeq      int
	restoringGroupID string
	restoreSeq       int
}

func (fleetPage *fleetPage) restoreInProgress() bool {
	return fleetPage.split.restoringGroupID != ""
}

func (fleetPage *fleetPage) beginGroupRestore(groupID string) int {
	fleetPage.split.restoreSeq++
	fleetPage.split.restoringGroupID = groupID
	return fleetPage.split.restoreSeq
}

func (fleetPage *fleetPage) finishGroupRestore(seq int) bool {
	if seq == 0 {
		return true
	}
	if seq != fleetPage.split.restoreSeq {
		return false
	}
	fleetPage.split.restoringGroupID = ""
	return true
}

// clearSplit resets every field that tracks the open split pane. Used
// whenever the split is closed (user toggle, external kill, restore
// teardown) so a future open starts from a known-empty state.
func (fleetPage *fleetPage) clearSplit() {
	// (The per-session open/close event log moved to the server, which owns
	// ~/.fleet/fleet.log; the split bookkeeping below is host-side only.)
	fleetPage.split.paneID = ""
	fleetPage.split.ref = InstanceRef{}
	fleetPage.split.session = ""
	fleetPage.split.openedAt = time.Time{}
	fleetPage.split.viaRestore = false
	fleetPage.split.activeGroup = ActiveGroup{}
	fleetPage.split.restoringGroupID = ""
}

// openInstanceSession opens a split pane for the given instance, reusing
// the last active session when available.
func (fleetPage *fleetPage) openInstanceSession(m *model, fleetName string, instance *fleet.Instance) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	ref := InstanceRef{Fleet: fleetName, Instance: instance.Name}
	sanitized := SanitizeSessionName(instance.Name)

	// Discovery is sourced from the server runtime; hitting enter on a collapsed
	// row with no lastActive entry would otherwise always spawn a new group.
	// Populate from the runtime cache on demand so we can attach to an existing
	// session when available.
	ensureSessionsLoaded(m, ref)

	// splitSessionCmd resolves the session shell argv (server-side) and builds a
	// split-pane command, surfacing a resolve error as a status message.
	splitSessionCmd := func(sessionName, groupID string) tea.Cmd {
		cols, rows := tmuxWindowSize()
		cols = cols * 70 / 100
		shellCmd := ShellCommandForSession(m.config, sessionName, cols, rows, true)
		cmd, err := attachExecCmd(fleetName, instance.Name, shellCmd)
		if err != nil {
			m.message = fmt.Sprintf("Could not open session: %v", err)
			return nil
		}
		return splitPaneCmd(fleetPage.split.paneID, ref, sessionName, groupID, cmd)
	}

	if last, ok := m.sessionStore.LastActive(ref); ok {
		if last.groupID != "" {
			return fleetPage.restoreGroupCmd(m, fleetName, instance, last.groupID)
		}
		return splitSessionCmd(last.sessionName, last.groupID)
	}

	if groups := m.sessionStore.Groups(ref); len(groups) > 0 {
		g := groups[0]
		rootName := g.Sessions[0].Name
		if g.GroupID != "" && isGroupedSession(sanitized, rootName) {
			return fleetPage.restoreGroupCmd(m, fleetName, instance, g.GroupID)
		}
		return splitSessionCmd(rootName, g.GroupID)
	}

	newGroupID := randomHex(3)
	sessName := GroupSessionName(sanitized, newGroupID)
	return splitSessionCmd(sessName, newGroupID)
}

// cycleSessionGroup moves the visual selection to the next or previous
// session group within the currently-split instance and starts a
// debounce timer. Group lookup is scoped to fleetPage.splitRef so two
// instances that share group IDs cannot leak into each other.
func (fleetPage *fleetPage) cycleSessionGroup(m *model, prev bool) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	groups := m.sessionStore.Groups(fleetPage.split.ref)
	if len(groups) < 2 {
		return nil
	}

	from := fleetPage.split.activeGroup
	if !fleetPage.split.pendingGroup.Empty() {
		from = fleetPage.split.pendingGroup
	}

	currentIdx := -1
	for i, g := range groups {
		if g.GroupID == from.GroupID && from.Ref == fleetPage.split.ref {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		return nil
	}

	targetIdx := currentIdx - 1
	if !prev {
		targetIdx = currentIdx + 1
	}
	if targetIdx < 0 {
		targetIdx = len(groups) - 1
	} else if targetIdx >= len(groups) {
		targetIdx = 0
	}

	fleetPage.split.pendingGroup = ActiveGroup{Ref: fleetPage.split.ref, GroupID: groups[targetIdx].GroupID}
	fleetPage.split.debounceSeq++
	return groupCycleDebounce(fleetPage.split.debounceSeq)
}

// commitGroupCycle performs the actual pane switch after the debounce
// timer expires.
func (fleetPage *fleetPage) commitGroupCycle(m *model) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	if fleetPage.split.pendingGroup.Empty() || fleetPage.split.pendingGroup == fleetPage.split.activeGroup {
		fleetPage.split.pendingGroup = ActiveGroup{}
		return nil
	}

	target := fleetPage.split.pendingGroup
	fleetPage.split.pendingGroup = ActiveGroup{}

	f, ok := m.st.Fleets[target.Ref.Fleet]
	if !ok {
		return nil
	}
	instance, err := f.GetInstance(target.Ref.Instance)
	if err != nil {
		return nil
	}

	fleetPage.saveCurrentGroupLayout(m)
	killAllSplitPanes()

	fleetPage.split.activeGroup = target

	return fleetPage.restoreGroupCmd(m, target.Ref.Fleet, instance, target.GroupID)
}

// ===========================================
// Backend Helpers
// ===========================================

package tui

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sessionGroup represents a set of related tmux sessions inside a
// container that should be displayed and managed as a unit. Sessions
// created by splitting the outer tmux all share the same group ID.
//
// Naming convention:
//
//	<instance>~<groupID>       — root session (first pane)
//	<instance>~<groupID>~<hex> — additional panes
//
// The '~' character acts as the group separator.
const groupSep = "~"

// sessionGroup is a collection of sessions sharing a group ID.
type sessionGroup struct {
	GroupID  string        // hex identifier, e.g. "a1b2c3"
	Sessions []tmuxSession // individual tmux sessions in the group
}

// savedGroup stores the state of a group's outer tmux panes so
// they can be restored when switching back.
type savedGroup struct {
	GroupID      string   // group identifier
	InstanceName string   // instance this group belongs to
	Sessions     []string // ordered session names (for pane recreation)
	Layout       string   // tmux layout string (for layout restoration)
	PaneCount    int      // number of shell panes (excluding TUI)
}

// computeGroupKey returns a composite key for the savedGroups map that
// uniquely identifies a group within a specific instance. This ensures
// that two instances with sessions sharing the same group ID do not
// collide in the map.
func computeGroupKey(instanceName, groupID string) string {
	return instanceName + "/" + groupID
}

// sameSavedGroup reports whether two savedGroup values are byte-identical.
// Used by saveCurrentGroupLayout to skip redundant writes on poll ticks
// when the user hasn't changed anything.
func sameSavedGroup(a, b savedGroup) bool {
	if a.GroupID != b.GroupID ||
		a.InstanceName != b.InstanceName ||
		a.Layout != b.Layout ||
		a.PaneCount != b.PaneCount ||
		len(a.Sessions) != len(b.Sessions) {
		return false
	}
	for i := range a.Sessions {
		if a.Sessions[i] != b.Sessions[i] {
			return false
		}
	}
	return true
}

// savedGroupPaneCount reports the number of panes the group is expected
// to restore. Sessions length is authoritative because save always sets
// PaneCount = len(Sessions); PaneCount is only consulted as a fallback
// for legacy state that recorded a count but no Sessions array.
func savedGroupPaneCount(sg savedGroup) int {
	if len(sg.Sessions) > 0 {
		return len(sg.Sessions)
	}
	if sg.PaneCount > 0 {
		return sg.PaneCount
	}
	return 1
}

// savedGroupSessionNames returns the persisted session order for a saved
// group, deduplicated. When Sessions is empty (legacy state that only
// recorded PaneCount) it falls back to a single root session — callers
// should never see an empty result. It deliberately does NOT synthesize
// names to pad up to PaneCount: doing that was the source of the "ghost
// pane" bug, because synthesized names get persisted and later restored
// as brand-new empty tmux sessions.
func savedGroupSessionNames(sg savedGroup, sanitizedInstance string) []string {
	sessions := make([]string, 0, len(sg.Sessions))
	seen := make(map[string]bool, len(sg.Sessions))
	for _, name := range sg.Sessions {
		if name == "" || seen[name] {
			continue
		}
		sessions = append(sessions, name)
		seen[name] = true
	}
	if len(sessions) == 0 {
		sessions = append(sessions, GroupSessionName(sanitizedInstance, sg.GroupID))
	}
	return sessions
}

// GroupSessionName builds the root session name for a group:
// <instance>~<groupID>. Exported because the CLI (fleet shell,
// spawn-session, …) must mint names with the exact same convention the
// TUI parses — a session created outside it surfaces as a pseudo-group
// and splits then duplicate it (see ResolveSessionName).
func GroupSessionName(sanitizedInstance, groupID string) string {
	return sanitizedInstance + groupSep + groupID
}

// NewGroupRootSessionName mints the root session name for a brand-new
// group with a random group ID.
func NewGroupRootSessionName(sanitizedInstance string) string {
	return GroupSessionName(sanitizedInstance, randomHex(3))
}

// NewGroupPaneSessionName mints a session name for an additional pane in
// an existing group: <instance>~<groupID>~<hex>.
func NewGroupPaneSessionName(sanitizedInstance, groupID string) string {
	return GroupSessionName(sanitizedInstance, groupID) + groupSep + randomHex(2)
}

// ResolveSessionName maps a user-supplied session name to its canonical
// grouped form for the given instance. Names that already follow the
// convention pass through unchanged; anything else becomes the root
// session of a group named after it: <instance>~<name>. This keeps the
// short names agents use with spawn-session/exec-in-session/read-session
// pointing at the same session the TUI manages.
func ResolveSessionName(instanceName, name string) string {
	sanitized := SanitizeSessionName(instanceName)
	if isGroupedSession(sanitized, name) {
		return name
	}
	return GroupSessionName(sanitized, SanitizeSessionName(name))
}

// parseGroupID extracts the group ID from a session name, if it follows
// the group naming convention. Returns ("", false) for non-grouped sessions.
func parseGroupID(sanitizedInstance, sessionName string) (groupID string, ok bool) {
	prefix := sanitizedInstance + groupSep
	if !strings.HasPrefix(sessionName, prefix) {
		return "", false
	}
	rest := sessionName[len(prefix):]
	// rest is either "groupID" or "groupID~suffix"
	parts := strings.SplitN(rest, groupSep, 2)
	if parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// groupSessions takes a list of discovered tmux sessions and groups them
// by group ID. Sessions that don't follow the naming convention are
// returned as individual groups (one session per group, using the session
// name as the group ID). Groups are sorted by group ID for stable display.
func groupSessions(sanitizedInstance string, sessions []tmuxSession) []sessionGroup {
	grouped := make(map[string]*sessionGroup)
	var ungrouped []tmuxSession

	for _, session := range sessions {
		gid, ok := parseGroupID(sanitizedInstance, session.Name)
		if !ok {
			ungrouped = append(ungrouped, session)
			continue
		}
		group, exists := grouped[gid]
		if !exists {
			group = &sessionGroup{GroupID: gid}
			grouped[gid] = group
		}
		group.Sessions = append(group.Sessions, session)
	}

	// Build result: ungrouped sessions first (as single-session groups),
	// then grouped sessions sorted by ID.
	var result []sessionGroup
	for _, session := range ungrouped {
		result = append(result, sessionGroup{
			GroupID:  session.Name, // use session name as pseudo-group ID
			Sessions: []tmuxSession{session},
		})
	}

	gids := make([]string, 0, len(grouped))
	for gid := range grouped {
		gids = append(gids, gid)
	}
	sort.Strings(gids)
	for _, gid := range gids {
		result = append(result, *grouped[gid])
	}

	return result
}

// isGroupedSession returns true if the session name follows the group
// naming convention for the given instance.
func isGroupedSession(sanitizedInstance, sessionName string) bool {
	_, ok := parseGroupID(sanitizedInstance, sessionName)
	return ok
}

// splitBindGroupID returns the group ID that is safe to pass to
// `fleet shell --group` when rebinding the outer tmux split keys after
// attaching to sessionName. It mirrors the isGroupedSession guard the
// attach path uses (page_fleet.go handleEnter): for a bare-named session
// the groupID is a pseudo-group ID (the session name itself), and
// passing it to --group would mint a <instance>~<name>~<hex> session — a
// duplicate real group named after the bare session. Returning "" makes
// a split start a fresh group instead.
func splitBindGroupID(instanceName, sessionName, groupID string) string {
	if !isGroupedSession(SanitizeSessionName(instanceName), sessionName) {
		return ""
	}
	return groupID
}

// groupCycleMsg is sent after the debounce timer expires to confirm
// a session group switch.
type groupCycleMsg struct {
	seq int // must match m.debounceSeq to be acted on
}

// groupCycleDebounce returns a tea.Cmd that fires a groupCycleMsg
// after 500ms. If additional pgup/pgdown presses arrive before it
// fires, the seq will have been incremented and this msg is ignored.
func groupCycleDebounce(seq int) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(_ time.Time) tea.Msg {
		return groupCycleMsg{seq: seq}
	})
}

// randomHex returns a random hex string of n bytes (2n characters).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

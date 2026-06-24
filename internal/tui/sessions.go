package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Types
// ===========================================

// tmuxSession represents a discovered tmux session inside a container.
type tmuxSession struct {
	Name     string
	Windows  int
	Attached bool
}

// sessionDiscovery holds the discovered sessions for a single instance, cached
// in the SessionStore. Sourced from the server runtime now (not a direct tmux
// exec); err is retained for stores that record a failed read.
type sessionDiscovery struct {
	sessions  []tmuxSession
	err       error
	fetchedAt time.Time
}

// splitInstanceKey splits a "<fleet>/<instance>" key on the first separator.
// Returns ok=false for a key with no separator.
func splitInstanceKey(key string) (fleetName, instanceName string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

// ===========================================
// Discovery (sourced from the server runtime)
// ===========================================
//
// The server polls tmux sessions for every running instance (~1s) and pushes
// them on the runtime sidecar (InstanceRuntime.Sessions). The TUI no longer
// execs tmux to list sessions itself — it reads the cached runtime. Live updates
// arrive via runtimeChangedMsg; on-demand reads (expand, post-op refresh) read
// the same cache synchronously.

// runtimeSessions converts the cached runtime session list for ref into the
// TUI's tmuxSession slice. Returns nil when no runtime is cached yet.
func (m *model) runtimeSessions(ref InstanceRef) []tmuxSession {
	r := m.runtime[rtKey(ref.Fleet, ref.Instance)]
	if r == nil {
		return nil
	}
	return protoSessionsToLegacy(r.GetSessions())
}

func protoSessionsToLegacy(in []*fleetgrpc.TmuxSession) []tmuxSession {
	if len(in) == 0 {
		return nil
	}
	out := make([]tmuxSession, 0, len(in))
	for _, s := range in {
		out = append(out, tmuxSession{
			Name:     s.GetName(),
			Windows:  int(s.GetWindows()),
			Attached: s.GetAttached(),
		})
	}
	return out
}

// refreshSessionsFromRuntime updates the session store for ref from the cached
// runtime, then prunes saved layouts whose groups no longer exist. Synchronous;
// safe to call on the Update goroutine. A no-op for an unexpanded ref.
func (m *model) refreshSessionsFromRuntime(ref InstanceRef) {
	if !ref.Valid() || !m.sessionStore.IsExpanded(ref) {
		return
	}
	m.sessionStore.SetDiscovery(ref, m.runtimeSessions(ref))
	m.pruneSavedGroupsForInstance(ref)
}

// ensureSessionsLoaded synchronously populates the session store for ref from
// the runtime when it has not been loaded yet. Needed before opening a session
// from a row that was never expanded, so the attach-vs-new decision can see
// existing tmux sessions instead of always spawning a new group.
func ensureSessionsLoaded(m *model, ref InstanceRef) {
	if m.sessionStore.HasDiscovery(ref) {
		return
	}
	m.sessionStore.SetDiscovery(ref, m.runtimeSessions(ref))
}

// ===========================================
// Mutating session commands (exec via the server)
// ===========================================
//
// Create / rename / delete mutate tmux inside the container through
// runInstanceCommand (exec_client.go): a local daemon resolves the exec argv
// and runs it locally (the P5 boundary), a remote one streams over the Exec
// RPC. The UI reflects the change on the next ~1s runtime tick (or the
// synchronous post-op refresh).

// runSessionScript runs a shell one-liner inside ref's container and folds a
// non-zero exit into the returned error — matching the semantics of the old
// local cmd.Run()/cmd.Output() call sites these commands were built on. The
// create scripts merge tmux's stderr into stdout (2>&1) so a captured reason
// (e.g. "duplicate session: NAME") rides along here instead of being lost to a
// bare "exit status 1" — the single biggest diagnosability win for the TUI,
// which otherwise swallowed why a session failed to create.
func runSessionScript(ref InstanceRef, script string) (string, error) {
	out, code, err := runInstanceCommand(ref.Fleet, ref.Instance, []string{"sh", "-c", script})
	if err != nil {
		return out, err
	}
	if code != 0 {
		if reason := strings.TrimSpace(out); reason != "" {
			return out, fmt.Errorf("exit status %d: %s", code, reason)
		}
		return out, fmt.Errorf("exit status %d", code)
	}
	return out, nil
}

// sessionCreatedMsg is sent after creating a new tmux session.
type sessionCreatedMsg struct {
	ref InstanceRef
	err error
}

// sessionRenamedMsg is sent after renaming a tmux session.
//
// oldGroupID/newGroupID are set only when a grouped session was renamed (the
// group ID itself changes, so every session in the group is reprefixed). They
// let the handler migrate in-memory state — savedGroups, the active group, the
// open split, and last-active — off the old group ID so a stale entry can't
// resurface as a duplicate row or strand the shell on a vanished session.
type sessionRenamedMsg struct {
	ref        InstanceRef
	oldName    string
	newName    string
	oldGroupID string
	newGroupID string
	err        error
}

// sessionDeletedMsg is sent after killing a tmux session (or group of sessions).
type sessionDeletedMsg struct {
	ref         InstanceRef
	sessionName string // root session name that was deleted
	groupID     string // non-empty when a full group was deleted
	err         error
}

// createSessionCmd ensures tmux is installed (matching the interactive shell
// path) and then creates a detached session inside the container.
func createSessionCmd(ref InstanceRef, sessionName string) tea.Cmd {
	return func() tea.Msg {
		// 2>&1 (not 2>/dev/null): on a name collision tmux exits 1 with
		// "duplicate session: NAME" on stderr — keep it so runSessionScript can
		// surface the reason instead of a bare "exit status 1".
		script := tmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s 2>&1`, shQuote(sessionName))
		if _, err := runSessionScript(ref, script); err != nil {
			return sessionCreatedMsg{ref: ref, err: err}
		}
		return sessionCreatedMsg{ref: ref}
	}
}

// presetSessionsCreatedMsg is sent after creating a session group from a
// layout preset (issue #150). On success the handler records the group's
// saved layout so opening the group restores the preset's pane geometry.
type presetSessionsCreatedMsg struct {
	ref      InstanceRef
	groupID  string
	sessions []string // position-ordered session names (slot i = pane i)
	layout   string   // the preset's tmux layout string ("" = default stacking)
	err      error
}

// buildPresetSessionScript builds the in-container one-liner that mints a
// preset's tmux sessions and types each pane's startup command into its
// session.
//
// Shape:
//
//	tmux new-session -d -s ROOT 2>&1 || exit 1
//	{ <root send-keys> && <pane new-sessions + send-keys> } || { <kill them all>; exit 1 }
//
// The root is created first and on its own: if THAT fails (e.g. the group name
// already exists), the script exits immediately and the cleanup never runs, so a
// pre-existing session is left untouched — never killed. Only once the root is
// ours does the rest run; if any later step fails, the cleanup tears down the
// root plus its panes. The caller mints the root under a fresh random group id,
// so the whole "<inst>~<group>~*" namespace is this run's — the kills can only
// hit sessions this run made, never a live group — and a partial chain can't
// strand a root that would collide forever on retry. Every tmux step uses
// 2>&1 so a failure at ANY step (not just new-session) keeps its reason (e.g.
// "duplicate session: NAME") for runSessionScript to surface, instead of a bare
// "exit status 1".
//
// send-keys targets use "=<name>:" — exact session match (the group's pane names
// extend the root name, so prefix matching would be ambiguous) and the trailing
// ":" makes it a session target; a bare "=<name>" fails target-pane parsing
// ("can't find pane") on tmux 3.x. -l sends the command literally so key names
// inside it (e.g. "Enter") are not interpreted, and the "--" terminates
// send-keys' own flag parsing so a command beginning with "-" (e.g. "-rf ...")
// is not mistaken for a flag.
func buildPresetSessionScript(sessions []string, commands []string) string {
	if len(sessions) == 0 {
		return ""
	}
	root := sessions[0]

	sendKeys := func(name, command string) []string {
		target := shQuote("=" + name + ":")
		return []string{
			fmt.Sprintf("tmux send-keys -t %s -l -- %s 2>&1", target, shQuote(command)),
			fmt.Sprintf("tmux send-keys -t %s Enter 2>&1", target),
		}
	}

	// Steps that run only after the root session exists.
	var inner []string
	if len(commands) > 0 && commands[0] != "" {
		inner = append(inner, sendKeys(root, commands[0])...)
	}
	for i := 1; i < len(sessions); i++ {
		inner = append(inner, fmt.Sprintf(`tmux new-session -d -s %s 2>&1`, shQuote(sessions[i])))
		if i < len(commands) && commands[i] != "" {
			inner = append(inner, sendKeys(sessions[i], commands[i])...)
		}
	}

	var b strings.Builder
	b.WriteString(tmuxEnsureInstalled)
	fmt.Fprintf(&b, `tmux new-session -d -s %s 2>&1`, shQuote(root))
	if len(inner) == 0 {
		return b.String()
	}

	kills := make([]string, 0, len(sessions))
	for _, name := range sessions {
		kills = append(kills, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null", shQuote("="+name+":")))
	}
	fmt.Fprintf(&b, " || exit 1\n{ %s; } || { %s; exit 1; }",
		strings.Join(inner, " && "), strings.Join(kills, "; "))
	return b.String()
}

// createSessionGroupFromPresetCmd creates every session of a preset-backed
// group inside the container (running each pane's startup command) and reports
// the outcome; the handler persists the group layout on success.
func createSessionGroupFromPresetCmd(ref InstanceRef, groupID string, sessions []string, preset fleet.LayoutPreset) tea.Cmd {
	script := buildPresetSessionScript(sessions, preset.PaneCommands)
	layout := preset.Layout
	return func() tea.Msg {
		if _, err := runSessionScript(ref, script); err != nil {
			return presetSessionsCreatedMsg{ref: ref, groupID: groupID, err: err}
		}
		return presetSessionsCreatedMsg{ref: ref, groupID: groupID, sessions: sessions, layout: layout}
	}
}

// renameSessionCmd execs `tmux rename-session -t <old> <new>` in the container.
func renameSessionCmd(ref InstanceRef, oldName, newName string) tea.Cmd {
	return func() tea.Msg {
		script := fmt.Sprintf(`tmux rename-session -t %s %s 2>/dev/null`, shQuote(oldName), shQuote(newName))
		if _, err := runSessionScript(ref, script); err != nil {
			return sessionRenamedMsg{ref: ref, oldName: oldName, newName: newName, err: err}
		}
		return sessionRenamedMsg{ref: ref, oldName: oldName, newName: newName}
	}
}

// renameGroupCmd renames all sessions in a group: it lists sessions matching the
// old group prefix, then renames each by swapping the old group ID for the new.
func renameGroupCmd(ref InstanceRef, sanitizedInstance, oldGroupID, newGroupID string) tea.Cmd {
	oldPrefix := sanitizedInstance + groupSep + oldGroupID
	newPrefix := sanitizedInstance + groupSep + newGroupID

	return func() tea.Msg {
		out, err := runSessionScript(ref, `tmux list-sessions -F "#{session_name}" 2>/dev/null`)
		if err != nil {
			return sessionRenamedMsg{ref: ref, oldName: oldPrefix, newName: newPrefix, err: err}
		}

		var lastErr error
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			name := strings.TrimSpace(line)
			if name == "" || !strings.HasPrefix(name, oldPrefix) {
				continue
			}
			renamed := newPrefix + name[len(oldPrefix):]
			script := fmt.Sprintf(`tmux rename-session -t %s %s 2>/dev/null`, shQuote(name), shQuote(renamed))
			if _, err := runSessionScript(ref, script); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return sessionRenamedMsg{ref: ref, oldName: oldPrefix, newName: newPrefix, oldGroupID: oldGroupID, newGroupID: newGroupID, err: lastErr}
		}
		return sessionRenamedMsg{ref: ref, oldName: oldPrefix, newName: newPrefix, oldGroupID: oldGroupID, newGroupID: newGroupID}
	}
}

// deleteSessionCmd kills a single tmux session inside the container.
func deleteSessionCmd(ref InstanceRef, sessionName string) tea.Cmd {
	return func() tea.Msg {
		script := fmt.Sprintf(`tmux kill-session -t %s 2>/dev/null`, shQuote(sessionName))
		if _, err := runSessionScript(ref, script); err != nil {
			return sessionDeletedMsg{ref: ref, sessionName: sessionName, err: err}
		}
		return sessionDeletedMsg{ref: ref, sessionName: sessionName}
	}
}

// deleteGroupSessionsCmd kills all tmux sessions belonging to a group: it lists
// sessions matching the group prefix and kills each one.
func deleteGroupSessionsCmd(ref InstanceRef, sanitizedInstance, groupID string) tea.Cmd {
	prefix := sanitizedInstance + groupSep + groupID

	return func() tea.Msg {
		out, err := runSessionScript(ref, `tmux list-sessions -F "#{session_name}" 2>/dev/null`)
		if err != nil {
			return sessionDeletedMsg{ref: ref, sessionName: prefix, groupID: groupID, err: err}
		}

		var lastErr error
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			name := strings.TrimSpace(line)
			if name == "" || !strings.HasPrefix(name, prefix) {
				continue
			}
			script := fmt.Sprintf(`tmux kill-session -t %s 2>/dev/null`, shQuote(name))
			if _, err := runSessionScript(ref, script); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return sessionDeletedMsg{ref: ref, sessionName: prefix, groupID: groupID, err: lastErr}
		}
		return sessionDeletedMsg{ref: ref, sessionName: prefix, groupID: groupID}
	}
}

// ===========================================
// Helpers
// ===========================================

// sessionStillExists checks whether a lastSession reference is still valid
// against the current list of discovered tmux sessions.
func sessionStillExists(last lastSession, sessions []tmuxSession) bool {
	if last.groupID != "" {
		for _, session := range sessions {
			if strings.Contains(session.Name, groupSep+last.groupID) {
				return true
			}
		}
		return false
	}
	for _, session := range sessions {
		if session.Name == last.sessionName {
			return true
		}
	}
	return false
}

// nextSessionName generates an auto-incrementing session name like "session-2".
func nextSessionName(existing []tmuxSession) string {
	maxN := 1
	for _, session := range existing {
		if strings.HasPrefix(session.Name, "session-") {
			if n, err := strconv.Atoi(strings.TrimPrefix(session.Name, "session-")); err == nil && n > maxN {
				maxN = n
			}
		}
	}
	return fmt.Sprintf("session-%d", maxN+1)
}

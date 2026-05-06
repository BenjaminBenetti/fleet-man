package tui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
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

// sessionDiscovery holds discovered sessions for a single instance.
type sessionDiscovery struct {
	sessions  []tmuxSession
	err       error
	fetchedAt time.Time
}

// ===========================================
// Messages
// ===========================================

// sessionsMsg is sent after listing tmux sessions inside a container.
type sessionsMsg struct {
	instanceKey string // "fleet/instance"
	sessions    []tmuxSession
	err         error
}

// sessionCreatedMsg is sent after creating a new tmux session.
type sessionCreatedMsg struct {
	instanceKey string
	err         error
}

// sessionRenamedMsg is sent after renaming a tmux session.
type sessionRenamedMsg struct {
	instanceKey string
	oldName     string
	newName     string
	err         error
}

// sessionDeletedMsg is sent after killing a tmux session (or group of sessions).
type sessionDeletedMsg struct {
	instanceKey string
	sessionName string // root session name that was deleted
	groupID     string // non-empty when a full group was deleted
	err         error
}

// sessionDiscoveryMsg carries discovered sessions for expanded instances.
type sessionDiscoveryMsg struct {
	discovered map[string][]tmuxSession // instanceKey → sessions
}

// sessionDiscoveryCmd lists tmux sessions for all expanded, running
// instances. Runs on a 1-second loop to detect external session
// creation/destruction.
func sessionDiscoveryCmd(
	backends map[fleet.BackendType]backend.Backend,
	expanded map[string]bool,
	fleets map[string]*fleet.Fleet,
) tea.Cmd {
	type target struct {
		instanceKey  string
		workspaceDir string
		backendType  fleet.BackendType
	}
	var targets []target
	for _, f := range fleets {
		for _, inst := range f.Instances {
			if inst.Status != fleet.StatusRunning || inst.ContainerID == "" {
				continue
			}
			bt := inst.Backend
			if bt == "" {
				bt = fleet.BackendDevcontainer
			}
			instKey := f.Name + "/" + inst.Name
			if !expanded[instKey] {
				continue
			}
			targets = append(targets, target{
				instanceKey:  instKey,
				workspaceDir: inst.WorkspaceDir,
				backendType:  bt,
			})
		}
	}

	return func() tea.Msg {
		time.Sleep(1 * time.Second)

		if len(targets) == 0 {
			return sessionDiscoveryMsg{}
		}

		discovered := make(map[string][]tmuxSession)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, t := range targets {
			b := backends[t.backendType]
			if b == nil {
				continue
			}
			wg.Add(1)
			go func(b backend.Backend, wsDir, instKey string) {
				defer wg.Done()
				cmd := b.ExecCommand(wsDir, []string{
					"sh", "-c",
					`tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`,
				})
				out, err := cmd.Output()
				if err != nil {
					// tmux exits with an error when no sessions exist
					// (server not running). Record an empty list so
					// stale sessions are cleared from the UI.
					mu.Lock()
					discovered[instKey] = nil
					mu.Unlock()
					return
				}
				sessions := parseTmuxSessions(string(out), sanitizedFromInstanceKey(instKey))
				mu.Lock()
				discovered[instKey] = sessions
				mu.Unlock()
			}(b, t.workspaceDir, t.instanceKey)
		}

		wg.Wait()
		return sessionDiscoveryMsg{discovered: discovered}
	}
}

// ensureSessionsLoaded synchronously populates m.sessions[instKey] when
// it has not been loaded yet. This is needed before opening an instance
// session from a row that was never expanded, so the attach-vs-new
// decision can see existing tmux sessions instead of always spawning a
// new group.
func ensureSessionsLoaded(m *model, b backend.Backend, workspaceDir, instanceKey string) {
	if _, ok := m.sessions[instanceKey]; ok {
		return
	}
	cmd := b.ExecCommand(workspaceDir, []string{
		"sh", "-c",
		`tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`,
	})
	out, err := cmd.Output()
	if err != nil {
		m.sessions[instanceKey] = &sessionDiscovery{err: err, fetchedAt: time.Now()}
		return
	}
	m.sessions[instanceKey] = &sessionDiscovery{
		sessions:  parseTmuxSessions(string(out), sanitizedFromInstanceKey(instanceKey)),
		fetchedAt: time.Now(),
	}
}

// listSessionsCmd returns a tea.Cmd that execs `tmux list-sessions`
// inside the container and parses the output into tmuxSession structs.
func listSessionsCmd(b backend.Backend, workspaceDir, instanceKey string) tea.Cmd {
	return func() tea.Msg {
		cmd := b.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`,
		})
		out, err := cmd.Output()
		if err != nil {
			return sessionsMsg{instanceKey: instanceKey, err: err}
		}
		sessions := parseTmuxSessions(string(out), sanitizedFromInstanceKey(instanceKey))
		return sessionsMsg{instanceKey: instanceKey, sessions: sessions}
	}
}

// createSessionCmd ensures tmux is installed (matching the interactive
// shell path) and then creates a detached session inside the container.
func createSessionCmd(b backend.Backend, workspaceDir, instanceKey, sessionName string) tea.Cmd {
	return func() tea.Msg {
		cmd := b.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			tmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s 2>/dev/null`, shQuote(sessionName)),
		})
		if err := cmd.Run(); err != nil {
			return sessionCreatedMsg{instanceKey: instanceKey, err: err}
		}
		return sessionCreatedMsg{instanceKey: instanceKey}
	}
}

// renameSessionCmd execs `tmux rename-session -t <old> <new>` inside
// the container.
func renameSessionCmd(b backend.Backend, workspaceDir, instanceKey, oldName, newName string) tea.Cmd {
	return func() tea.Msg {
		cmd := b.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			fmt.Sprintf(`tmux rename-session -t %s %s 2>/dev/null`, shQuote(oldName), shQuote(newName)),
		})
		if err := cmd.Run(); err != nil {
			return sessionRenamedMsg{instanceKey: instanceKey, oldName: oldName, newName: newName, err: err}
		}
		return sessionRenamedMsg{instanceKey: instanceKey, oldName: oldName, newName: newName}
	}
}

// renameGroupCmd renames all sessions in a group. It lists sessions
// matching the old group prefix, then renames each one by swapping the
// old group ID for the new one.
func renameGroupCmd(b backend.Backend, workspaceDir, instanceKey, sanitizedInstance, oldGroupID, newGroupID string) tea.Cmd {
	oldPrefix := sanitizedInstance + groupSep + oldGroupID
	newPrefix := sanitizedInstance + groupSep + newGroupID

	return func() tea.Msg {
		// List all sessions in the container.
		listCmd := b.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}" 2>/dev/null`,
		})
		out, err := listCmd.Output()
		if err != nil {
			return sessionRenamedMsg{instanceKey: instanceKey, oldName: oldPrefix, newName: newPrefix, err: err}
		}

		// Rename each session that matches the old group prefix.
		var lastErr error
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" || !strings.HasPrefix(name, oldPrefix) {
				continue
			}
			// Swap prefix: instance~oldGID~suffix → instance~newGID~suffix
			renamed := newPrefix + name[len(oldPrefix):]
			cmd := b.ExecCommand(workspaceDir, []string{
				"sh", "-c",
				fmt.Sprintf(`tmux rename-session -t %s %s 2>/dev/null`, shQuote(name), shQuote(renamed)),
			})
			if err := cmd.Run(); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return sessionRenamedMsg{instanceKey: instanceKey, oldName: oldPrefix, newName: newPrefix, err: lastErr}
		}
		return sessionRenamedMsg{instanceKey: instanceKey, oldName: oldPrefix, newName: newPrefix}
	}
}

// deleteSessionCmd kills a single tmux session inside the container.
func deleteSessionCmd(b backend.Backend, workspaceDir, instanceKey, sessionName string) tea.Cmd {
	return func() tea.Msg {
		cmd := b.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			fmt.Sprintf(`tmux kill-session -t %s 2>/dev/null`, shQuote(sessionName)),
		})
		if err := cmd.Run(); err != nil {
			return sessionDeletedMsg{instanceKey: instanceKey, sessionName: sessionName, err: err}
		}
		return sessionDeletedMsg{instanceKey: instanceKey, sessionName: sessionName}
	}
}

// deleteGroupSessionsCmd kills all tmux sessions belonging to a group.
// It lists sessions matching the group prefix and kills each one.
func deleteGroupSessionsCmd(b backend.Backend, workspaceDir, instanceKey, sanitizedInstance, groupID string) tea.Cmd {
	prefix := sanitizedInstance + groupSep + groupID

	return func() tea.Msg {
		// List all sessions in the container.
		listCmd := b.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}" 2>/dev/null`,
		})
		out, err := listCmd.Output()
		if err != nil {
			return sessionDeletedMsg{instanceKey: instanceKey, sessionName: prefix, groupID: groupID, err: err}
		}

		// Kill each session that matches the group prefix.
		var lastErr error
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" || !strings.HasPrefix(name, prefix) {
				continue
			}
			cmd := b.ExecCommand(workspaceDir, []string{
				"sh", "-c",
				fmt.Sprintf(`tmux kill-session -t %s 2>/dev/null`, shQuote(name)),
			})
			if err := cmd.Run(); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return sessionDeletedMsg{instanceKey: instanceKey, sessionName: prefix, groupID: groupID, err: lastErr}
		}
		return sessionDeletedMsg{instanceKey: instanceKey, sessionName: prefix, groupID: groupID}
	}
}

// ===========================================
// Helpers
// ===========================================

// sessionStillExists checks whether a lastSession reference is still
// valid against the current list of discovered tmux sessions. For
// grouped sessions it looks for any session with the group prefix;
// for ungrouped sessions it matches the exact name.
func sessionStillExists(last lastSession, sessions []tmuxSession) bool {
	if last.groupID != "" {
		// Group session: check if any session has the group prefix.
		for _, s := range sessions {
			if strings.Contains(s.Name, groupSep+last.groupID) {
				return true
			}
		}
		return false
	}
	for _, s := range sessions {
		if s.Name == last.sessionName {
			return true
		}
	}
	return false
}

// parseTmuxSessions parses the output of `tmux list-sessions -F
// "#{session_name}:#{session_windows}:#{session_attached}"` into
// a slice of tmuxSession scoped to a single instance.
//
// Only sessions whose name equals sanitizedInstance (legacy non-grouped
// shell session) or starts with sanitizedInstance + groupSep are
// returned. Containerized backends naturally only have fleet-managed
// sessions on their per-container tmux server, so this filter is a
// no-op for them. For local_dir the host tmux server is shared, and
// the prefix filter is what keeps unrelated host sessions (the fleet
// TUI's own outer tmux, the user's other work) out of the instance view.
func parseTmuxSessions(output string, sanitizedInstance string) []tmuxSession {
	var sessions []tmuxSession
	prefix := sanitizedInstance + groupSep
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 1 || parts[0] == "" {
			continue
		}
		name := parts[0]
		if name != sanitizedInstance && !strings.HasPrefix(name, prefix) {
			continue
		}
		s := tmuxSession{Name: name}
		if len(parts) >= 2 {
			s.Windows, _ = strconv.Atoi(parts[1])
		}
		if len(parts) >= 3 {
			s.Attached = parts[2] == "1"
		}
		sessions = append(sessions, s)
	}
	return sessions
}

// sanitizedFromInstanceKey returns the sanitized instance name from an
// "<fleet>/<instance>" key, suitable for matching tmux session prefixes.
func sanitizedFromInstanceKey(instanceKey string) string {
	if i := strings.LastIndex(instanceKey, "/"); i >= 0 {
		return SanitizeSessionName(instanceKey[i+1:])
	}
	return SanitizeSessionName(instanceKey)
}

// nextSessionName generates an auto-incrementing session name like
// "session-2", "session-3", etc. based on existing sessions.
func nextSessionName(existing []tmuxSession) string {
	maxN := 1
	for _, s := range existing {
		if strings.HasPrefix(s.Name, "session-") {
			if n, err := strconv.Atoi(strings.TrimPrefix(s.Name, "session-")); err == nil && n > maxN {
				maxN = n
			}
		}
	}
	return fmt.Sprintf("session-%d", maxN+1)
}

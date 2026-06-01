package tui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
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
	ref      InstanceRef
	sessions []tmuxSession
	err      error
}

// sessionCreatedMsg is sent after creating a new tmux session.
type sessionCreatedMsg struct {
	ref InstanceRef
	err error
}

// sessionRenamedMsg is sent after renaming a tmux session.
type sessionRenamedMsg struct {
	ref     InstanceRef
	oldName string
	newName string
	err     error
}

// sessionDeletedMsg is sent after killing a tmux session (or group of sessions).
type sessionDeletedMsg struct {
	ref         InstanceRef
	sessionName string // root session name that was deleted
	groupID     string // non-empty when a full group was deleted
	err         error
}

// sessionDiscoveryMsg carries discovered sessions for expanded instances.
type sessionDiscoveryMsg struct {
	discovered map[InstanceRef][]tmuxSession
}

// sessionDiscoveryCmd lists tmux sessions for all expanded, running
// instances. Runs on a 1-second loop to detect external session
// creation/destruction.
func sessionDiscoveryCmd(
	backends map[fleet.BackendType]backend.Backend,
	expanded []InstanceRef,
	fleets map[string]*fleet.Fleet,
) tea.Cmd {
	type target struct {
		ref          InstanceRef
		workspaceDir string
		backendType  fleet.BackendType
	}
	expandedSet := make(map[InstanceRef]bool, len(expanded))
	for _, ref := range expanded {
		expandedSet[ref] = true
	}
	var targets []target
	for _, f := range fleets {
		for _, instance := range f.Instances {
			if instance.Status != fleet.StatusRunning || instance.ContainerID == "" {
				continue
			}
			backendType := instance.Backend
			if backendType == "" {
				backendType = fleet.BackendDevcontainer
			}
			ref := InstanceRef{Fleet: f.Name, Instance: instance.Name}
			if !expandedSet[ref] {
				continue
			}
			targets = append(targets, target{
				ref:          ref,
				workspaceDir: instance.WorkspaceDir,
				backendType:  backendType,
			})
		}
	}

	return func() tea.Msg {
		time.Sleep(1 * time.Second)

		if len(targets) == 0 {
			return sessionDiscoveryMsg{}
		}

		discovered := make(map[InstanceRef][]tmuxSession)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, t := range targets {
			instanceBackend := backends[t.backendType]
			if instanceBackend == nil {
				continue
			}
			wg.Add(1)
			go func(instanceBackend backend.Backend, wsDir string, ref InstanceRef) {
				defer wg.Done()
				cmd := instanceBackend.ExecCommandQuiet(wsDir, []string{
					"sh", "-c",
					`tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`,
				})
				out, err := cmd.Output()
				if err != nil {
					// tmux exits with an error when no sessions exist
					// (server not running). Record an empty list so
					// stale sessions are cleared from the UI.
					mu.Lock()
					discovered[ref] = nil
					mu.Unlock()
					return
				}
				sessions := parseTmuxSessions(string(out))
				mu.Lock()
				discovered[ref] = sessions
				mu.Unlock()
			}(instanceBackend, t.workspaceDir, t.ref)
		}

		wg.Wait()
		return sessionDiscoveryMsg{discovered: discovered}
	}
}

// ensureSessionsLoaded synchronously populates the session store for
// ref when it has not been loaded yet. Needed before opening an
// instance session from a row that was never expanded, so the
// attach-vs-new decision can see existing tmux sessions instead of
// always spawning a new group.
func ensureSessionsLoaded(m *model, instanceBackend backend.Backend, workspaceDir string, ref InstanceRef) {
	if m.sessionStore.HasDiscovery(ref) {
		return
	}
	cmd := instanceBackend.ExecCommandQuiet(workspaceDir, []string{
		"sh", "-c",
		`tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`,
	})
	out, err := cmd.Output()
	if err != nil {
		m.sessionStore.SetDiscoveryError(ref, err)
		return
	}
	m.sessionStore.SetDiscovery(ref, parseTmuxSessions(string(out)))
}

// listSessionsCmd returns a tea.Cmd that execs `tmux list-sessions`
// inside the container and parses the output into tmuxSession structs.
func listSessionsCmd(instanceBackend backend.Backend, workspaceDir string, ref InstanceRef) tea.Cmd {
	return func() tea.Msg {
		cmd := instanceBackend.ExecCommandQuiet(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`,
		})
		out, err := cmd.Output()
		if err != nil {
			return sessionsMsg{ref: ref, err: err}
		}
		sessions := parseTmuxSessions(string(out))
		return sessionsMsg{ref: ref, sessions: sessions}
	}
}

// createSessionCmd ensures tmux is installed (matching the interactive
// shell path) and then creates a detached session inside the container.
func createSessionCmd(instanceBackend backend.Backend, workspaceDir string, ref InstanceRef, sessionName string) tea.Cmd {
	return func() tea.Msg {
		cmd := instanceBackend.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			tmuxEnsureInstalled + fmt.Sprintf(`tmux new-session -d -s %s 2>/dev/null`, shQuote(sessionName)),
		})
		start := time.Now()
		if err := cmd.Run(); err != nil {
			flog.Error("session create failed", "fleet", ref.Fleet, "instance", ref.Instance, "session", sessionName, "ms", flog.MillisSince(start), "err", err)
			return sessionCreatedMsg{ref: ref, err: err}
		}
		flog.Info("session created", "fleet", ref.Fleet, "instance", ref.Instance, "session", sessionName, "ms", flog.MillisSince(start))
		return sessionCreatedMsg{ref: ref}
	}
}

// logSessionOpen records a TUI session being opened, including the raw shell
// command run inside the container and how it is surfaced: "attach" (suspends
// the TUI, runs in the foreground), "split" (a tmux split pane), or "terminal"
// (a separate terminal emulator). These paths run the command outside the
// backend.Cmd wrapper, so they are instrumented here by hand. Only "attach"
// has a matching close event (logSessionClose) and therefore a duration; split
// panes and external terminals detach and keep running, so they are logged
// without one. The command is the inner command (what runs in the container),
// not the backend's exec wrapper.
func logSessionOpen(mode, fleetName, instanceName, sessionName, command string) {
	flog.Info("session opened", "fleet", fleetName, "instance", instanceName, "session", sessionName, "mode", mode, "cmd", command)
}

// logSessionClose records a foreground session attach ending, with how long it
// was attached. Called from the tea.ExecProcess completion callback, which
// fires when the user detaches or the shell exits.
func logSessionClose(fleetName, instanceName, sessionName string, start time.Time) {
	flog.Info("session closed", "fleet", fleetName, "instance", instanceName, "session", sessionName, "ms", flog.MillisSince(start))
}

// renameSessionCmd execs `tmux rename-session -t <old> <new>` inside
// the container.
func renameSessionCmd(instanceBackend backend.Backend, workspaceDir string, ref InstanceRef, oldName, newName string) tea.Cmd {
	return func() tea.Msg {
		cmd := instanceBackend.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			fmt.Sprintf(`tmux rename-session -t %s %s 2>/dev/null`, shQuote(oldName), shQuote(newName)),
		})
		start := time.Now()
		if err := cmd.Run(); err != nil {
			return sessionRenamedMsg{ref: ref, oldName: oldName, newName: newName, err: err}
		}
		flog.Info("session renamed", "fleet", ref.Fleet, "instance", ref.Instance, "from", oldName, "to", newName, "ms", flog.MillisSince(start))
		return sessionRenamedMsg{ref: ref, oldName: oldName, newName: newName}
	}
}

// renameGroupCmd renames all sessions in a group. It lists sessions
// matching the old group prefix, then renames each one by swapping the
// old group ID for the new one.
func renameGroupCmd(instanceBackend backend.Backend, workspaceDir string, ref InstanceRef, sanitizedInstance, oldGroupID, newGroupID string) tea.Cmd {
	oldPrefix := sanitizedInstance + groupSep + oldGroupID
	newPrefix := sanitizedInstance + groupSep + newGroupID

	return func() tea.Msg {
		start := time.Now()
		// List all sessions in the container.
		listCmd := instanceBackend.ExecCommandQuiet(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}" 2>/dev/null`,
		})
		out, err := listCmd.Output()
		if err != nil {
			return sessionRenamedMsg{ref: ref, oldName: oldPrefix, newName: newPrefix, err: err}
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
			cmd := instanceBackend.ExecCommand(workspaceDir, []string{
				"sh", "-c",
				fmt.Sprintf(`tmux rename-session -t %s %s 2>/dev/null`, shQuote(name), shQuote(renamed)),
			})
			if err := cmd.Run(); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return sessionRenamedMsg{ref: ref, oldName: oldPrefix, newName: newPrefix, err: lastErr}
		}
		flog.Info("session group renamed", "fleet", ref.Fleet, "instance", ref.Instance, "from", oldPrefix, "to", newPrefix, "ms", flog.MillisSince(start))
		return sessionRenamedMsg{ref: ref, oldName: oldPrefix, newName: newPrefix}
	}
}

// deleteSessionCmd kills a single tmux session inside the container.
func deleteSessionCmd(instanceBackend backend.Backend, workspaceDir string, ref InstanceRef, sessionName string) tea.Cmd {
	return func() tea.Msg {
		cmd := instanceBackend.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			fmt.Sprintf(`tmux kill-session -t %s 2>/dev/null`, shQuote(sessionName)),
		})
		start := time.Now()
		if err := cmd.Run(); err != nil {
			return sessionDeletedMsg{ref: ref, sessionName: sessionName, err: err}
		}
		flog.Info("session killed", "fleet", ref.Fleet, "instance", ref.Instance, "session", sessionName, "ms", flog.MillisSince(start))
		return sessionDeletedMsg{ref: ref, sessionName: sessionName}
	}
}

// deleteGroupSessionsCmd kills all tmux sessions belonging to a group.
// It lists sessions matching the group prefix and kills each one.
func deleteGroupSessionsCmd(instanceBackend backend.Backend, workspaceDir string, ref InstanceRef, sanitizedInstance, groupID string) tea.Cmd {
	prefix := sanitizedInstance + groupSep + groupID

	return func() tea.Msg {
		start := time.Now()
		// List all sessions in the container.
		listCmd := instanceBackend.ExecCommandQuiet(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}" 2>/dev/null`,
		})
		out, err := listCmd.Output()
		if err != nil {
			return sessionDeletedMsg{ref: ref, sessionName: prefix, groupID: groupID, err: err}
		}

		// Kill each session that matches the group prefix.
		var lastErr error
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" || !strings.HasPrefix(name, prefix) {
				continue
			}
			cmd := instanceBackend.ExecCommand(workspaceDir, []string{
				"sh", "-c",
				fmt.Sprintf(`tmux kill-session -t %s 2>/dev/null`, shQuote(name)),
			})
			if err := cmd.Run(); err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return sessionDeletedMsg{ref: ref, sessionName: prefix, groupID: groupID, err: lastErr}
		}
		flog.Info("session group killed", "fleet", ref.Fleet, "instance", ref.Instance, "group", groupID, "ms", flog.MillisSince(start))
		return sessionDeletedMsg{ref: ref, sessionName: prefix, groupID: groupID}
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

// parseTmuxSessions parses the output of `tmux list-sessions -F
// "#{session_name}:#{session_windows}:#{session_attached}"` into
// a slice of tmuxSession.
func parseTmuxSessions(output string) []tmuxSession {
	var sessions []tmuxSession
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 1 || parts[0] == "" {
			continue
		}
		session := tmuxSession{Name: parts[0]}
		if len(parts) >= 2 {
			session.Windows, _ = strconv.Atoi(parts[1])
		}
		if len(parts) >= 3 {
			session.Attached = parts[2] == "1"
		}
		sessions = append(sessions, session)
	}
	return sessions
}

// nextSessionName generates an auto-incrementing session name like
// "session-2", "session-3", etc. based on existing sessions.
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

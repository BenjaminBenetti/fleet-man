package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// splitPaneMsg is sent after a tmux split-window command completes.
type splitPaneMsg struct {
	paneID   string // tmux pane ID (e.g. "%3")
	fleet    string // fleet name for the instance
	instance string // instance name occupying the pane
	session  string // tmux session name in the pane
	groupID  string // session group ID (for group management)
	err      error
}

// tmuxWindowSize queries the host tmux for the current window dimensions.
// Returns (cols, rows) or (0, 0) if the query fails.
func tmuxWindowSize() (int, int) {
	out, err := exec.Command("tmux", "display-message", "-p", "#{window_width} #{window_height}").Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return 0, 0
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return w, h
}

// quoteArgs builds a shell-safe command string from exec args.
func quoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

// splitPaneCmd creates or replaces the right-side tmux pane with the given
// command. When an existing pane ID is provided, it is respawned in-place
// (via respawn-pane) to avoid layout changes that cause visual corruption.
// If the pane no longer exists, it falls back to creating a fresh split.
func splitPaneCmd(existingPaneID string, fleetName string, instanceName string, sessionName string, groupID string, cmd *exec.Cmd) tea.Cmd {
	// Snapshot the args — we must not capture the *exec.Cmd across goroutines.
	args := cmd.Args

	return func() tea.Msg {
		// Wrap the command so that on non-zero exit the pane stays open
		// briefly, giving the user time to read the error message.
		shellScript := quoteArgs(args) + `; __rc=$?; if [ $__rc -ne 0 ]; then echo; echo "exited with code $__rc — closing in 3s"; sleep 3; fi; exit $__rc`

		// If we have an existing pane, respawn it in-place to avoid
		// layout changes that cause visual corruption in the fleet TUI.
		if existingPaneID != "" {
			respawnArgs := []string{
				"respawn-pane", "-k",
				"-t", existingPaneID,
				"sh", "-c", shellScript,
			}
			if exec.Command("tmux", respawnArgs...).Run() == nil {
				_ = exec.Command("tmux", "select-pane", "-t", existingPaneID).Run()
				return splitPaneMsg{paneID: existingPaneID, fleet: fleetName, instance: instanceName, session: sessionName, groupID: groupID}
			}
			// Pane is gone — fall through to create a fresh split.
		}

		// Kill any stale sibling panes before creating a fresh split.
		// Must select the TUI pane first — `kill-pane -a` kills every
		// pane except the focused one, and rapid switches can leave a
		// child shell pane focused, which would kill the TUI itself.
		killAllSplitPanes()

		// Create a horizontal split (side by side). -P -F prints the new
		// pane ID so we can track it. -l 70% gives the shell pane 70% width.
		tmuxArgs := []string{
			"split-window", "-h",
			"-l", "70%",
			"-P", "-F", "#{pane_id}",
			"--", "sh", "-c", shellScript,
		}
		out, err := exec.Command("tmux", tmuxArgs...).Output()
		if err != nil {
			return splitPaneMsg{err: fmt.Errorf("split-window: %w", err)}
		}

		paneID := strings.TrimSpace(string(out))
		_ = exec.Command("tmux", "select-pane", "-t", paneID).Run()
		return splitPaneMsg{paneID: paneID, fleet: fleetName, instance: instanceName, session: sessionName, groupID: groupID}
	}
}

// splitOpen returns true if the current tmux window has more than one
// pane, meaning the split is still visible. More reliable than checking
// a specific pane ID, which can go stale.
func splitOpen() bool {
	out, err := exec.Command("tmux", "list-panes", "-F", "x").Output()
	if err != nil {
		return false
	}
	return strings.Count(strings.TrimSpace(string(out)), "x") > 1
}

// killSplitPane kills the tracked tmux pane if it exists. Safe to call
// with an empty paneID (no-op).
func killSplitPane(paneID string) {
	if paneID == "" {
		return
	}
	_ = exec.Command("tmux", "kill-pane", "-t", paneID).Run()
}

// listPaneSlots returns pane IDs sorted by screen position (top then
// left), skipping the TUI pane. Used after layout application to
// determine which pane occupies which visual slot.
func listPaneSlots() []string {
	panes := listPanesByPosition()
	var ids []string
	for _, p := range panes {
		ids = append(ids, p.id)
	}
	return ids
}

// bindHostSplitKeys rebinds the outer tmux's % and " keys so that
// new splits open a shell inside the given instance (via fleet shell)
// instead of spawning a local shell. When groupID is non-empty, new
// panes are added to the same session group.
func bindHostSplitKeys(instanceName, groupID string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	shellCmd := fmt.Sprintf("%s shell %s", self, instanceName)
	if groupID != "" {
		shellCmd += fmt.Sprintf(" --group %s", groupID)
	}
	_ = exec.Command("tmux", "bind-key", "%", "split-window", "-h", shellCmd).Run()
	_ = exec.Command("tmux", "bind-key", `"`, "split-window", "-v", shellCmd).Run()
}

// unbindHostSplitKeys restores the default tmux split-window bindings.
func unbindHostSplitKeys() {
	_ = exec.Command("tmux", "bind-key", "%", "split-window", "-h").Run()
	_ = exec.Command("tmux", "bind-key", `"`, "split-window", "-v").Run()
}

// unbindDefaultSplitKeys disables the default split-window bindings so
// the user doesn't accidentally create host shell panes before selecting
// an instance. A display-message reminds them to use the TUI.
func unbindDefaultSplitKeys() {
	msg := "Select an instance in fleet first"
	_ = exec.Command("tmux", "bind-key", "%", "display-message", msg).Run()
	_ = exec.Command("tmux", "bind-key", `"`, "display-message", msg).Run()
}

// splitWindowWithRetry runs `tmux split-window` with the given args,
// retrying up to 3 times with a 250ms pause between attempts. tmux
// occasionally fails a split mid-layout under rapid pane churn — a
// short backoff lets the server settle. Returns the trimmed stdout
// (the new pane's ID, captured via -P -F) on success, or the last
// error after all attempts fail.
func splitWindowWithRetry(args []string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		out, err := exec.Command("tmux", args...).Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
		lastErr = err
	}
	return "", lastErr
}

// killAllSplitPanes kills all panes except the TUI pane (index 0).
// Selects the TUI pane first so kill-pane -a removes the right targets
// regardless of which pane currently has focus.
func killAllSplitPanes() {
	_ = exec.Command("tmux", "select-pane", "-t", ":.0").Run()
	_ = exec.Command("tmux", "kill-pane", "-a").Run()
}

// bindHostCloseKeys binds Ctrl+Q and Ctrl+O on the outer tmux root
// table to close all split panes (select TUI pane, then kill others).
func bindHostCloseKeys() {
	// Use run-shell to chain tmux commands reliably. The guard
	// prevents errors when there's only the TUI pane.
	script := `tmux select-pane -t :.0 && tmux kill-pane -a 2>/dev/null || true`
	_ = exec.Command("tmux", "bind-key", "-n", "C-q", "run-shell", script).Run()
	_ = exec.Command("tmux", "bind-key", "-n", "C-o", "run-shell", script).Run()
}

// unbindHostCloseKeys removes the Ctrl+Q and Ctrl+O bindings from the
// outer tmux root table.
func unbindHostCloseKeys() {
	_ = exec.Command("tmux", "unbind", "-n", "C-q").Run()
	_ = exec.Command("tmux", "unbind", "-n", "C-o").Run()
}

// bindRefocusTUIKey binds prefix + Shift+H to refocus the TUI pane
// (index 0 of the current window) from any split. Uses the prefix key
// table rather than a root binding so it composes cleanly with other
// tmux bindings and works across every terminal.
func bindRefocusTUIKey() {
	_ = exec.Command("tmux", "bind-key", "H", "select-pane", "-t", ":.0").Run()
}

// unbindRefocusTUIKey removes the prefix + Shift+H binding.
func unbindRefocusTUIKey() {
	_ = exec.Command("tmux", "unbind", "H").Run()
}

// layoutTickMsg fires the fast layout poll. Unlike sessionDiscoveryMsg
// (which pays a ~2-3s round-trip to devcontainer exec), this tick only
// queries the outer tmux server — cheap local IPC — so it can run at
// 250ms to catch adds/kills before the user presses Ctrl+Q.
type layoutTickMsg struct{}

// layoutTickCmd returns a command that fires layoutTickMsg after 250ms.
// The handler self-reschedules so the tick runs as long as the program
// is alive; idle ticks are no-ops once the diff gate short-circuits.
func layoutTickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg {
		return layoutTickMsg{}
	})
}

// tmuxLayoutString returns the current tmux window layout string.
func tmuxLayoutString() string {
	out, err := exec.Command("tmux", "display-message", "-p", "#{window_layout}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// paneByPosition holds a tmux pane's screen coordinates, ID, and title.
type paneByPosition struct {
	top, left int
	id        string
	title     string
}

// listPanesByPosition returns non-TUI panes sorted by screen position
// (top then left). This gives a stable ordering that matches what the
// user sees, regardless of pane creation order or index numbering.
func listPanesByPosition() []paneByPosition {
	out, err := exec.Command("tmux", "list-panes", "-F",
		"#{pane_id}:#{pane_top}:#{pane_left}:#{pane_title}").Output()
	if err != nil {
		return nil
	}

	// Identify the TUI pane (index 0) so we can skip it.
	tuiID := ""
	if idOut, err := exec.Command("tmux", "display-message", "-t", ":.0", "-p", "#{pane_id}").Output(); err == nil {
		tuiID = strings.TrimSpace(string(idOut))
	}

	var panes []paneByPosition
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: %id:top:left:title
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 4 {
			continue
		}
		id := parts[0]
		if id == tuiID {
			continue
		}
		top, err1 := strconv.Atoi(parts[1])
		left, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			continue
		}
		panes = append(panes, paneByPosition{
			top: top, left: left,
			id: id, title: parts[3],
		})
	}

	sort.Slice(panes, func(i, j int) bool {
		if panes[i].top != panes[j].top {
			return panes[i].top < panes[j].top
		}
		return panes[i].left < panes[j].left
	})
	return panes
}

// paneSessionOrder returns session names (from pane titles) sorted by
// screen position. This gives a stable ordering for save/restore.
func paneSessionOrder() []string {
	panes := listPanesByPosition()
	var sessions []string
	for _, p := range panes {
		if p.title != "" {
			sessions = append(sessions, p.title)
		}
	}
	return sessions
}

// saveCurrentGroupLayout saves the active group's outer tmux layout so
// it can be restored later. Pane titles (set by fleet shell) are read
// in pane index order to preserve the session-to-position mapping. When
// st is non-nil the layout is also mirrored into state.json so it
// survives a fleet restart.
func (fleetPage *fleetPage) saveCurrentGroupLayout(st *state.State) {
	if fleetPage.activeGroupID == "" || fleetPage.splitInstance == "" {
		return
	}

	// Read session names from outer tmux pane titles, in pane order.
	sessionNames := paneSessionOrder()

	// No shell panes visible in the outer tmux means either the group
	// hasn't opened yet or its panes have already been killed (Ctrl+Q,
	// external kill, teardown mid-quit). Either way there's nothing
	// truthful to save — bail before we clobber a previously good
	// snapshot with a zero- or fallback-sized record.
	if len(sessionNames) == 0 {
		return
	}

	groupSnapshot := savedGroup{
		GroupID:      fleetPage.activeGroupID,
		InstanceName: fleetPage.splitInstance,
		Sessions:     sessionNames,
		Layout:       tmuxLayoutString(),
		PaneCount:    len(sessionNames),
	}

	// No-op when nothing changed. The 250ms layout tick fires this
	// constantly; without this gate an idle split would rewrite
	// state.json every tick with identical bytes.
	if existing, ok := fleetPage.savedGroups[fleetPage.activeGroupID]; ok && sameSavedGroup(existing, groupSnapshot) {
		return
	}
	fleetPage.savedGroups[fleetPage.activeGroupID] = groupSnapshot

	if st == nil {
		return
	}
	if st.GroupLayouts == nil {
		st.GroupLayouts = make(map[string]state.GroupLayout)
	}
	st.GroupLayouts[fleetPage.activeGroupID] = state.GroupLayout{
		GroupID:      groupSnapshot.GroupID,
		InstanceName: groupSnapshot.InstanceName,
		Sessions:     groupSnapshot.Sessions,
		Layout:       groupSnapshot.Layout,
		PaneCount:    groupSnapshot.PaneCount,
	}
	_ = state.Save(st)
}

// restoreGroupCmd recreates outer tmux panes for a saved session group.
// Instead of trusting the saved session list (which may be stale), it
// queries the inner tmux directly for sessions matching the group prefix.
// Each discovered session gets its own pane via `fleet shell --session`.
func (fleetPage *fleetPage) restoreGroupCmd(m *model, fleetName string, instance *fleet.Instance, groupID string) tea.Cmd {
	instanceBackend := m.instanceBackend(instance)
	instanceName := instance.Name
	qualifiedName := fleetName + "/" + instanceName
	workspaceDir := instance.WorkspaceDir
	sanitized := SanitizeSessionName(instanceName)
	prefix := sanitized + groupSep + groupID

	// Grab saved layout if available.
	savedLayout := ""
	if groupSnapshot, ok := fleetPage.savedGroups[groupID]; ok {
		savedLayout = groupSnapshot.Layout
	}

	// Prefer saved session order (from pane titles) to preserve
	// the exact pane-to-session mapping.
	var savedOrder []string
	if groupSnapshot, ok := fleetPage.savedGroups[groupID]; ok && len(groupSnapshot.Sessions) > 0 {
		savedOrder = groupSnapshot.Sessions
	}

	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return splitPaneMsg{err: fmt.Errorf("os.Executable: %w", err)}
		}

		// Query the inner tmux for all sessions in this group.
		listCmd := instanceBackend.ExecCommand(workspaceDir, []string{
			"sh", "-c",
			`tmux list-sessions -F "#{session_name}" 2>/dev/null`,
		})
		out, _ := listCmd.Output()

		// Build a set of live sessions for validation.
		live := make(map[string]bool)
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name != "" && strings.HasPrefix(name, prefix) {
				live[name] = true
			}
		}

		// Use saved order if available, filtering to sessions that
		// still exist. Then append any newly discovered sessions
		// that weren't in the saved order.
		var sessions []string
		seen := make(map[string]bool)
		for _, s := range savedOrder {
			if live[s] {
				sessions = append(sessions, s)
				seen[s] = true
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name != "" && strings.HasPrefix(name, prefix) && !seen[name] {
				sessions = append(sessions, name)
			}
		}

		if len(sessions) == 0 {
			return splitPaneMsg{err: fmt.Errorf("no sessions found for group %s", groupID)}
		}

		// Kill any existing split panes. Must select the TUI pane first —
		// `kill-pane -a` kills every pane except the focused one, and a
		// previous switch may have left a child shell pane focused.
		killAllSplitPanes()

		// Phase 1: Create placeholder panes with `sleep` so we can
		// establish the layout geometry without worrying about which
		// session goes where. Each split is retried (see
		// splitWindowWithRetry) because tmux can transiently refuse
		// splits during rapid pane churn. If a pane still can't be
		// created after retries, we record the error, skip that slot,
		// and surface the failure via splitPaneMsg.err so the user
		// sees it in the status line — Phases 2 and 3 still run on
		// whatever panes did succeed.
		var firstPaneID string
		var lastPaneID string
		var splitErr error
		for i := range sessions {
			var tmuxArgs []string
			if i == 0 {
				tmuxArgs = []string{
					"split-window", "-h",
					"-l", "70%",
					"-P", "-F", "#{pane_id}",
					"--", "sleep", "infinity",
				}
			} else {
				tmuxArgs = []string{
					"split-window", "-v",
					"-t", lastPaneID,
					"-P", "-F", "#{pane_id}",
					"--", "sleep", "infinity",
				}
			}
			paneID, err := splitWindowWithRetry(tmuxArgs)
			if err != nil {
				splitErr = err
				continue
			}
			lastPaneID = paneID
			if i == 0 {
				firstPaneID = paneID
			}
		}

		// Phase 2: Apply the saved layout to resize/reposition panes.
		if savedLayout != "" && firstPaneID != "" {
			_ = exec.Command("tmux", "select-layout", savedLayout).Run()
		}

		// Phase 3: Read the actual pane index→ID mapping after layout,
		// then respawn each pane with the correct session based on
		// position. This guarantees the right session is in the right
		// pane regardless of how tmux reordered indices.
		paneSlots := listPaneSlots()
		for i, sessName := range sessions {
			if i >= len(paneSlots) {
				break
			}
			pid := paneSlots[i]
			shellCmd := fmt.Sprintf("%s shell %s --session %s", self, qualifiedName, sessName)
			script := shellCmd + `; __rc=$?; if [ $__rc -ne 0 ]; then echo; echo "exited with code $__rc — closing in 3s"; sleep 3; fi; exit $__rc`
			_ = exec.Command("tmux", "respawn-pane", "-k", "-t", pid, "sh", "-c", script).Run()
		}

		if firstPaneID != "" {
			_ = exec.Command("tmux", "select-pane", "-t", firstPaneID).Run()
		}

		// Force a repaint after a brief delay to avoid blank/corrupted
		// terminals after rapid pane creation. Done in a goroutine so
		// the splitPaneMsg returns immediately.
		go func() {
			time.Sleep(2 * time.Second)
			_ = exec.Command("tmux", "refresh-client").Run()
		}()

		firstSession := ""
		if len(sessions) > 0 {
			firstSession = sessions[0]
		}
		msg := splitPaneMsg{
			paneID:   firstPaneID,
			fleet:    fleetName,
			instance: instanceName,
			session:  firstSession,
			groupID:  groupID,
		}
		if splitErr != nil {
			msg.err = fmt.Errorf("failed to do tmux split pane: %w", splitErr)
		}
		return msg
	}
}

// bindHostSessionCycleKeys binds Ctrl+PageUp/Down on the outer tmux to
// focus the TUI pane and send PageUp/PageDown, which the TUI handles
// as "cycle to previous/next session group".
func bindHostSessionCycleKeys() {
	_ = exec.Command("tmux", "bind-key", "-n", "C-PPage",
		"run-shell", "tmux select-pane -t :.0 && tmux send-keys -t :.0 PPage").Run()
	_ = exec.Command("tmux", "bind-key", "-n", "C-NPage",
		"run-shell", "tmux select-pane -t :.0 && tmux send-keys -t :.0 NPage").Run()
}

// unbindHostSessionCycleKeys restores the default C-PPage/C-NPage
// bindings on the host tmux.
func unbindHostSessionCycleKeys() {
	_ = exec.Command("tmux", "bind", "-T", "root", "C-PPage", "copy-mode", "-eu").Run()
	_ = exec.Command("tmux", "bind", "-T", "root", "C-NPage", "send-keys", "PPage").Run()
}

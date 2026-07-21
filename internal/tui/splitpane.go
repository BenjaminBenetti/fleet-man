package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/sys/unix"
)

// splitPaneMsg is sent after a tmux split-window command completes.
type splitPaneMsg struct {
	paneID     string      // tmux pane ID (e.g. "%3")
	ref        InstanceRef // instance occupying the pane
	session    string      // tmux session name in the pane
	groupID    string      // session group ID (for group management)
	command    string      // command(s) launched in the pane(s); for the event log
	restoreSeq int         // async restore token; zero means not a group restore
	err        error
}

// currentTermSize reads the TUI's own pty dimensions via ioctl (TIOCGWINSZ on
// stdout) — the authoritative size of the terminal bubbletea renders into, and
// exactly what bubbletea's SIGWINCH handler reads. This is the fleet *pane*
// size (not the whole tmux window), so it stays correct even when the TUI is
// only occupying part of a split window. Returns (cols, rows) or (0, 0) on
// failure.
func currentTermSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
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
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return width, height
}

// quoteArgs builds a shell-safe command string from exec args.
func quoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// splitPaneCmd creates or replaces the right-side tmux pane with the given
// command. When an existing pane ID is provided, it is respawned in-place
// (via respawn-pane) to avoid layout changes that cause visual corruption.
// If the pane no longer exists, it falls back to creating a fresh split.
func splitPaneCmd(existingPaneID string, ref InstanceRef, sessionName string, groupID string, cmd *exec.Cmd) tea.Cmd {
	// Snapshot the args — we must not capture the *exec.Cmd across goroutines.
	args := cmd.Args
	cmdStr := strings.Join(args, " ")

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
				tagPaneTitle(existingPaneID, sessionName)
				_ = exec.Command("tmux", "select-pane", "-t", existingPaneID).Run()
				return splitPaneMsg{paneID: existingPaneID, ref: ref, session: sessionName, groupID: groupID, command: cmdStr}
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
		tagPaneTitle(paneID, sessionName)
		_ = exec.Command("tmux", "select-pane", "-t", paneID).Run()
		return splitPaneMsg{paneID: paneID, ref: ref, session: sessionName, groupID: groupID, command: cmdStr}
	}
}

// tagPaneTitle sets the outer-tmux pane title to the session name the
// TUI just spawned. Required so that derivePersistableSnapshot can
// reliably map panes back to their sessions: `fleet shell` does this
// inside its own startup path, but the TUI bypasses the CLI for the
// first pane and during Phase-3 restore, so without this call those
// panes would report the runner hostname as their title and the strict
// save would bail forever.
func tagPaneTitle(paneID, sessionName string) {
	if paneID == "" || sessionName == "" {
		return
	}
	_ = exec.Command("tmux", "select-pane", "-t", paneID, "-T", sessionName).Run()
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

// placeholderSleepSecs keeps Phase-1 placeholder panes alive until they are
// respawned with their real session. It must be a plain seconds count:
// `sleep infinity` is a GNU coreutils extension, and macOS/BSD sleep rejects
// it and exits immediately — the pane then closes before the layout apply and
// respawn phases run, breaking every group restore/switch on macOS.
const placeholderSleepSecs = "2147483647"

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
	// Chain both commands into a single tmux invocation (`;` is tmux's
	// command separator when passed as its own argument) to halve the
	// subprocess spawns.
	_ = exec.Command("tmux", "select-pane", "-t", ":.0", ";", "kill-pane", "-a").Run()
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

// derivePersistableSnapshot turns the live outer-tmux pane state into a
// savedGroup that's safe to persist. It returns ok=false whenever any
// pane fails strict validation — an empty title, a title that doesn't
// parse as a session in the active group, a parsed groupID that doesn't
// match active, or a duplicate title across panes. The 250ms layout
// tick fires this constantly, so a transient ok=false (e.g. a pane that
// hasn't been tagged by `fleet shell` yet) is just a "retry next tick"
// signal — the in-memory snapshot from a previous successful tick is
// preserved.
//
// This is the strict replacement for PR #42's normalizeSavedGroupSessions,
// which fabricated synthetic `~restored##` session names from unparseable
// pane titles. Those fakes then got persisted and later restored as
// brand-new empty tmux sessions, surfacing as ghost panes.
func derivePersistableSnapshot(activeGroup ActiveGroup, panes []paneByPosition, layout string) (savedGroup, bool) {
	if activeGroup.Empty() || len(panes) == 0 {
		return savedGroup{}, false
	}
	sanitized := SanitizeSessionName(activeGroup.Ref.Instance)
	groupID := activeGroup.GroupID

	sessionNames := make([]string, 0, len(panes))
	seen := make(map[string]bool, len(panes))
	for _, p := range panes {
		if p.title == "" {
			return savedGroup{}, false
		}
		if !sessionInGroup(sanitized, p.title, groupID) {
			return savedGroup{}, false
		}
		if seen[p.title] {
			return savedGroup{}, false
		}
		sessionNames = append(sessionNames, p.title)
		seen[p.title] = true
	}

	return savedGroup{
		GroupID:      groupID,
		InstanceName: activeGroup.Ref.Instance,
		Sessions:     sessionNames,
		Layout:       layout,
		PaneCount:    len(sessionNames),
	}, true
}

// snapshotMatchesRuntime reports whether the snapshot's session set is
// exactly the set of live inner-tmux sessions for the group (per the
// server's runtime poll). A mismatch means the local outer tmux is not
// showing the group as it factually exists in the container — either
// another TUI changed the group (issue #158) or a local pane add/kill
// hasn't round-tripped through the ~1s runtime poll yet.
func snapshotMatchesRuntime(snapshot savedGroup, liveSessions []tmuxSession) bool {
	sanitized := SanitizeSessionName(snapshot.InstanceName)
	live := make(map[string]bool)
	for _, s := range liveSessions {
		if sessionInGroup(sanitized, s.Name, snapshot.GroupID) {
			live[s.Name] = true
		}
	}
	// snapshot.Sessions is duplicate-free (derivePersistableSnapshot
	// rejects duplicate pane titles), so length + membership is set
	// equality.
	if len(live) != len(snapshot.Sessions) {
		return false
	}
	for _, name := range snapshot.Sessions {
		if !live[name] {
			return false
		}
	}
	return true
}

// saveCurrentGroupLayout saves the active group's outer tmux layout so
// it can be restored later. Pane titles (set by `fleet shell`) are read
// in pane index order to preserve the session-to-position mapping. When
// m.st is non-nil the layout is also mirrored into the server state so
// it survives a fleet restart.
func (fleetPage *fleetPage) saveCurrentGroupLayout(m *model) {
	if fleetPage.split.activeGroup.Empty() {
		return
	}
	if fleetPage.restoreInProgress() {
		return
	}

	groupSnapshot, ok := derivePersistableSnapshot(
		fleetPage.split.activeGroup,
		listPanesByPosition(),
		tmuxLayoutString(),
	)
	if !ok {
		return
	}

	// No-op when nothing changed. The 250ms layout tick fires this
	// constantly; without this gate an idle split would rewrite
	// state.json every tick with identical bytes.
	key := computeGroupKey(groupSnapshot.InstanceName, groupSnapshot.GroupID)
	if existing, ok := fleetPage.savedGroups[key]; ok && sameSavedGroup(existing, groupSnapshot) {
		return
	}

	// Stale-view guard (issue #158): with two TUIs on the same fleetd,
	// each one's outer tmux is only a *view* of the group — the inner
	// tmux sessions are the shared truth. Persisting a snapshot that
	// contradicts the live session set would clobber newer state written
	// by the other TUI (and the two would then ping-pong). Skip without
	// updating the diff-gate cache so a legitimate local change (whose
	// runtime echo lags by up to ~1s) is retried on a later tick.
	if !snapshotMatchesRuntime(groupSnapshot, m.runtimeSessions(fleetPage.split.activeGroup.Ref)) {
		return
	}
	fleetPage.savedGroups[key] = groupSnapshot

	st := m.st
	if st == nil {
		return
	}
	if st.GroupLayouts == nil {
		st.GroupLayouts = make(map[string]configutil.GroupLayout)
	}
	// Use composite key (instanceName/groupID) for state persistence
	// to ensure isolation between instances with the same group ID.
	stateKey := computeGroupKey(groupSnapshot.InstanceName, groupSnapshot.GroupID)
	layout := configutil.GroupLayout{
		GroupID:      groupSnapshot.GroupID,
		InstanceName: groupSnapshot.InstanceName,
		Sessions:     groupSnapshot.Sessions,
		Layout:       groupSnapshot.Layout,
		PaneCount:    groupSnapshot.PaneCount,
	}
	st.GroupLayouts[stateKey] = layout
	_ = setGroupLayoutRemote(layout)
}

// restoreGroupCmd recreates outer tmux panes for a saved session group.
// Instead of trusting the saved session list (which may be stale), it
// queries the inner tmux directly for sessions matching the group prefix.
// Each discovered session gets its own pane via `fleet shell --session`.
func (fleetPage *fleetPage) restoreGroupCmd(m *model, fleetName string, instance *fleet.Instance, groupID string) tea.Cmd {
	restoreSeq := fleetPage.beginGroupRestore(groupID)
	instanceName := instance.Name
	qualifiedName := fleetName + "/" + instanceName
	sanitized := SanitizeSessionName(instanceName)

	// The live session list comes from the server runtime (it polls tmux for all
	// running instances), captured here on the Update goroutine and passed into
	// the closure as newline-delimited names — the same shape the old `tmux
	// list-sessions` exec produced, which restoreSessionNames filters by group.
	var runtimeNames []string
	for _, s := range m.runtimeSessions(InstanceRef{Fleet: fleetName, Instance: instanceName}) {
		runtimeNames = append(runtimeNames, s.Name)
	}
	sessionList := strings.Join(runtimeNames, "\n")

	// Grab the saved snapshot if available. Captured here, on the Update
	// goroutine, because savedGroups is also written there (layout tick,
	// watch reconcile) — reading it inside the closure below would race.
	// Saved session order (from pane titles) preserves the exact
	// pane-to-session mapping.
	//
	// The server's persisted copy (m.st, kept fresh by the Watch stream)
	// is preferred over this TUI's savedGroups cache. The cache can lag
	// behind another TUI's writes — e.g. while this group was exempt
	// from the watch reconcile as this TUI's open split — and restoring
	// from a stale session list resurrects killed sessions via
	// new-session -A (issue #158). The group being restored is never the
	// one currently open here (toggle-close handles that), and local
	// saves write m.st synchronously, so the server copy is
	// fresher-or-equal for any group reaching this path.
	key := computeGroupKey(instanceName, groupID)
	savedLayout := ""
	var savedOrder []string
	var savedSnapshot *savedGroup
	if m.st != nil {
		if gl, ok := m.st.GroupLayouts[key]; ok {
			savedSnapshot = &savedGroup{
				GroupID:      gl.GroupID,
				InstanceName: gl.InstanceName,
				Sessions:     gl.Sessions,
				Layout:       gl.Layout,
				PaneCount:    gl.PaneCount,
			}
		}
	}
	if savedSnapshot == nil {
		if sg, ok := fleetPage.savedGroups[key]; ok {
			savedSnapshot = &sg
		}
	}
	if savedSnapshot != nil {
		savedLayout = savedSnapshot.Layout
		if len(savedSnapshot.Sessions) > 0 {
			savedOrder = savedSnapshot.Sessions
		}
	}

	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return splitPaneMsg{restoreSeq: restoreSeq, err: fmt.Errorf("os.Executable: %w", err)}
		}

		sessions := restoreSessionNames(sessionList, groupID, savedOrder, savedSnapshot, sanitized)
		if len(sessions) == 0 {
			if _, ok := fleetPage.savedGroups[key]; !ok {
				return splitPaneMsg{restoreSeq: restoreSeq, err: fmt.Errorf("no sessions found for group %s", groupID)}
			}
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
					"--", "sleep", placeholderSleepSecs,
				}
			} else {
				tmuxArgs = []string{
					"split-window", "-v",
					"-t", lastPaneID,
					"-P", "-F", "#{pane_id}",
					"--", "sleep", placeholderSleepSecs,
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
		//
		// We tag the pane title with the session name BEFORE respawning,
		// not after — derivePersistableSnapshot's strict check runs on
		// every 250ms layoutTick and would otherwise bail until each
		// `fleet shell` child got far enough to call `tmux select-pane
		// -T`. The pre-tag closes the race deterministically.
		paneSlots := listPaneSlots()
		var ranCmds []string
		// Batch the per-pane tag + respawn (and the final focus) into one
		// chained tmux invocation (`;` is tmux's command separator). This
		// collapses ~2N+1 subprocess spawns into a single one. tmux runs the
		// chained commands in order, so each pane is still tagged
		// (select-pane -T) before it's respawned — the ordering
		// derivePersistableSnapshot relies on (see Phase 3 note above).
		var batch []string
		appendCmd := func(parts ...string) {
			if len(batch) > 0 {
				batch = append(batch, ";")
			}
			batch = append(batch, parts...)
		}
		for i, sessName := range sessions {
			if i >= len(paneSlots) {
				break
			}
			paneID := paneSlots[i]
			shellCmd := fmt.Sprintf("%s shell %s --session %s", self, qualifiedName, sessName)
			ranCmds = append(ranCmds, shellCmd)
			script := shellCmd + `; __rc=$?; if [ $__rc -ne 0 ]; then echo; echo "exited with code $__rc — closing in 3s"; sleep 3; fi; exit $__rc`
			if sessName != "" {
				appendCmd("select-pane", "-t", paneID, "-T", sessName)
			}
			appendCmd("respawn-pane", "-k", "-t", paneID, "sh", "-c", script)
		}
		if firstPaneID != "" {
			appendCmd("select-pane", "-t", firstPaneID)
		}
		if len(batch) > 0 {
			_ = exec.Command("tmux", batch...).Run()
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
			paneID:     firstPaneID,
			ref:        InstanceRef{Fleet: fleetName, Instance: instanceName},
			session:    firstSession,
			groupID:    groupID,
			command:    strings.Join(ranCmds, " ; "),
			restoreSeq: restoreSeq,
		}
		if splitErr != nil {
			msg.err = fmt.Errorf("failed to do tmux split pane: %w", splitErr)
		}
		return msg
	}
}

func restoreSessionNames(discovered, groupID string, savedOrder []string, savedSnapshot *savedGroup, sanitized string) []string {
	// Build a set of live sessions for validation. Membership is
	// boundary-aware (sessionInGroup, not a raw string prefix) so a sibling
	// group whose ID has this group's ID as a prefix — e.g. "dog-2" when
	// restoring "dog" — does not leak its sessions in as extra panes.
	live := make(map[string]bool)
	var liveOrder []string
	for _, line := range strings.Split(strings.TrimSpace(discovered), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && sessionInGroup(sanitized, name, groupID) {
			live[name] = true
			liveOrder = append(liveOrder, name)
		}
	}

	// A saved layout drives the restore: it records the pane count and
	// session-to-pane order the user left behind. Each restored `fleet
	// shell --session` uses tmux new-session -A (attach-or-create), so
	// restoring a name that is no longer alive CREATES it. That
	// recreate-if-dead is wanted in exactly one case: the whole group
	// died together (fleet/instance restart — no live group session
	// remains), where the snapshot is the only record of the panes to
	// bring back. When the group still has live sessions, a snapshot
	// entry missing from the live set was deliberately killed — a
	// restart would have killed the survivors too — and recreating it
	// resurrects a ghost pane that the layout tick then persists as
	// real state (issue #158). So with a live group the snapshot only
	// contributes pane order, and the live set decides membership:
	// dead entries are dropped and live extras (panes added by another
	// TUI) are appended, exactly like the savedOrder path below.
	if savedSnapshot != nil {
		ordered := savedGroupSessionNames(*savedSnapshot, sanitized)
		if len(live) == 0 {
			return ordered
		}
		savedOrder = ordered
	}

	// Use saved order if available, filtering to sessions that still
	// exist. Then append any newly discovered sessions that weren't in
	// the saved order.
	var sessions []string
	seen := make(map[string]bool)
	for _, s := range savedOrder {
		if live[s] {
			sessions = append(sessions, s)
			seen[s] = true
		}
	}
	for _, name := range liveOrder {
		if !seen[name] {
			sessions = append(sessions, name)
			seen[name] = true
		}
	}

	return sessions
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

package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ship.go is "shipping" a PR: with the PR status selected, s starts a "ship"
// tmux session that boots Claude Code and types `/ship <PR URL>` into its
// prompt, after which Claude carries on as a normal session. The session records the PR in its @fleet_ship
// tmux option; the daemon reports that back on the session list
// (TmuxSession.ship_pr_url) and probes the PR's state by URL
// (PrStatus.ships), so the row wears a blue gradient and a ship icon while
// shipping and a green one once the PR merged.

const (
	// shipGroupBase is the ship session's group ID; later ships in the same
	// instance take ship-2, ship-3, ….
	shipGroupBase = "ship"
	// shipOption is the tmux session option a ship session keeps its PR URL in.
	// backend.ListSessionsScript and the daemon's PR probe both read it.
	shipOption = "@fleet_ship"
	// shipIcon trails a ship session's name.
	shipIcon = "🚢"
)

// fleetShipsWithClaude reports whether the fleet has the Claude Code agent
// enabled, which shipping needs.
func (m *model) fleetShipsWithClaude(fleetName string) bool {
	f, ok := m.st.Fleets[fleetName]
	return ok && f.Settings.ClaudeCodeMount
}

// shippablePRRefs returns the open PRs behind a PR-carrying row: the ones a ship
// can start on. A closed or merged PR has nothing left to ship.
func (m *model) shippablePRRefs(r row) []*fleetgrpc.PrRef {
	if !r.prStatusInline || r.instance == nil {
		return nil
	}
	ps := m.runtime[rtKey(r.fleetName, r.instance.Name)].GetPrStatus()
	if ps.GetOpenCount() == 0 {
		return nil
	}
	return ps.GetPrs()
}

// canShipRow reports whether s would ship from this row, for the help bar.
func (m *model) canShipRow(r row) bool {
	return m.fleetShipsWithClaude(r.fleetName) && len(m.shippablePRRefs(r)) > 0
}

// shipSelectedPR ships the PR of the selected PR status: directly when there is
// one open PR, or through the chooser when there are several.
func (fleetPage *fleetPage) shipSelectedPR(m *model) tea.Cmd {
	r := fleetPage.currentRow()
	if r == nil || len(m.rowInlinePRRefs(*r)) == 0 {
		return nil
	}
	if !m.fleetShipsWithClaude(r.fleetName) {
		m.message = "Shipping needs Claude Code: enable it for this fleet (e on the fleet → Agents)"
		return nil
	}
	refs := m.shippablePRRefs(*r)
	switch len(refs) {
	case 0:
		m.message = "Nothing to ship: the PR is closed or merged"
		return nil
	case 1:
		return fleetPage.startShip(m, r.fleetName, r.instance.Name, refs[0])
	default:
		fleetPage.choosePR = choosePRState{
			refs:      refs,
			ship:      true,
			fleetName: r.fleetName,
			instance:  r.instance.Name,
		}
		fleetPage.mode = viewChoosePR
		return nil
	}
}

// startShip starts the ship session for ref in the instance, unless one is
// already shipping that PR.
func (fleetPage *fleetPage) startShip(m *model, fleetName, instanceName string, ref *fleetgrpc.PrRef) tea.Cmd {
	if ref.GetUrl() == "" {
		return nil
	}
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return nil
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil || inst.Status != fleet.StatusRunning {
		m.message = "Instance must be running to ship"
		return nil
	}
	iref := InstanceRef{Fleet: fleetName, Instance: instanceName}
	ensureSessionsLoaded(m, iref)
	sessions := m.sessionStore.Sessions(iref)
	sanitized := SanitizeSessionName(instanceName)
	for _, s := range sessions {
		if s.ShipURL == ref.GetUrl() {
			label := s.Name
			if gid, ok := parseGroupID(sanitized, s.Name); ok {
				label = gid
			}
			m.message = fmt.Sprintf("PR #%d is already shipping in %s", ref.GetNumber(), label)
			return nil
		}
	}
	groupID := shipGroupID(sanitized, sessions)
	m.message = fmt.Sprintf("Shipping PR #%d…", ref.GetNumber())
	return startShipCmd(iref, GroupSessionName(sanitized, groupID), groupID, ref)
}

// shipGroupID picks the ship session's group ID: "ship", or the first free
// "ship-N" when a live session already holds it.
func shipGroupID(sanitizedInstance string, existing []tmuxSession) string {
	taken := make(map[string]bool, len(existing))
	for _, s := range existing {
		if gid, ok := parseGroupID(sanitizedInstance, s.Name); ok {
			taken[gid] = true
		}
	}
	if !taken[shipGroupBase] {
		return shipGroupBase
	}
	for n := 2; ; n++ {
		if id := fmt.Sprintf("%s-%d", shipGroupBase, n); !taken[id] {
			return id
		}
	}
}

// shipLaunch is typed into the ship session's shell: Claude Code, interactive.
// /ship runs only from a line typed into Claude's prompt box, not from a
// prompt on Claude's command line, so the prompt is typed in once the box is
// up (shipPromptScript).
const shipLaunch = "claude"

// shipPromptWait bounds, in seconds, how long a ship session waits for
// Claude's prompt box before giving up on typing /ship into it: long enough
// for a first start and for the user to answer a folder-trust prompt.
const shipPromptWait = 300

// shipPromptPrefix starts the line typed into Claude's prompt box; the PR URL
// follows it.
const shipPromptPrefix = "/ship "

// shipPromptReadyAWK exits 0 when a captured pane shows Claude Code's prompt
// box: an input line starting with ❯ (> in the older boxed UI) between two
// rules of ─ (with ╭╮/╰╯ corners and │ sides in that UI). A folder-trust
// prompt's "❯ 1. Yes, proceed" and a shell prompt drawn as ❯ have no rule on
// both sides. Multi-byte glyphs appear only as whole literals or groups, so
// it reads the same in a byte (C locale) awk as in a UTF-8 one.
const shipPromptReadyAWK = `function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }
function rule(s) { s = trim(s); sub(/^(╭|╰)/, "", s); sub(/(╮|╯)$/, "", s); return length(s) >= 30 && s ~ /^(─)+$/ }
function input(s) { s = trim(s); sub(/^│ */, "", s); return index(s, "❯") == 1 || index(s, ">") == 1 }
{ l[NR] = $0 }
END { for (i = 2; i < NR; i++) if (input(l[i]) && rule(l[i-1]) && rule(l[i+1])) exit 0; exit 1 }`

// shipPromptScript is the waiter the ship session runs inside the instance's
// tmux server (run-shell -b), so it outlives the command that started it and
// the TUI: every second it captures the session's pane until Claude's prompt
// box shows, then types shipPromptPrefix and the session's @fleet_ship URL, with
// Enter sent a moment later (an Enter inside the same burst would land in the
// prompt as part of a paste). It gives up after shipPromptWait seconds, or as
// soon as the session is gone; it never types into anything but the prompt
// box. The tmux it calls is the server's own (TMUX names its socket; the
// server is its parent, should tmux not be on PATH).
func shipPromptScript(session string) string {
	sess := shQuote("=" + session)
	target := shQuote("=" + session + ":")
	return fmt.Sprintf(`exec >/dev/null 2>&1
tm=tmux; command -v tmux || tm=/proc/$PPID/exe
url=$("$tm" show-options -v -t %[2]s %[3]s) && [ -n "$url" ] || exit 0
i=0
while [ "$i" -lt %[4]d ]; do
  "$tm" has-session -t %[1]s || exit 0
  if "$tm" capture-pane -p -t %[2]s | awk %[5]s; then
    "$tm" send-keys -t %[2]s -l -- %[6]s"$url" && sleep 1 && "$tm" send-keys -t %[2]s Enter
    exit 0
  fi
  sleep 1
  i=$((i+1))
done`, sess, target, shipOption, shipPromptWait, shQuote(shipPromptReadyAWK), shQuote(shipPromptPrefix))
}

// buildShipSessionScript builds the in-container one-liner that creates the ship
// session, tags it with the PR URL, starts shipPromptScript in the tmux server,
// and types shipLaunch into the session's shell. The session keeps a shell
// under Claude, so quitting Claude leaves a normal session. Like
// buildPresetSessionScript, the session is created on its own first (a name
// collision exits without touching the existing session), later failures kill
// only the session this run made, targets are "=<name>:" (exact session
// match), and every step keeps its stderr for runSessionScript. run-shell
// expands formats in its command, so the waiter's '#'s are doubled.
func buildShipSessionScript(session, url string) string {
	target := shQuote("=" + session + ":")
	waiter := shQuote(strings.ReplaceAll(shipPromptScript(session), "#", "##"))
	return tmuxEnsureInstalled +
		fmt.Sprintf(`tmux new-session -d -s %s 2>&1 || exit 1`, shQuote(session)) + "\n" +
		fmt.Sprintf(`{ tmux set-option -t %s %s %s 2>&1 && tmux run-shell -b %s 2>&1 && tmux send-keys -t %s -l -- %s 2>&1 && tmux send-keys -t %s Enter 2>&1; } || { tmux kill-session -t %s 2>/dev/null; exit 1; }`,
			target, shipOption, shQuote(url),
			waiter,
			target, shQuote(shipLaunch),
			target,
			target)
}

// shipStartedMsg reports the outcome of starting a ship session.
type shipStartedMsg struct {
	ref     InstanceRef
	groupID string
	number  int32
	err     error
}

// startShipCmd creates the ship session inside the instance.
func startShipCmd(ref InstanceRef, session, groupID string, pr *fleetgrpc.PrRef) tea.Cmd {
	script := buildShipSessionScript(session, pr.GetUrl())
	number := pr.GetNumber()
	return func() tea.Msg {
		_, err := runSessionScript(ref, script)
		return shipStartedMsg{ref: ref, groupID: groupID, number: number, err: err}
	}
}

// handleShipStarted reports a finished ship start and refreshes the session
// list so the ship row shows up without waiting for the next runtime tick.
func (m *model) handleShipStarted(msg shipStartedMsg) {
	if msg.err != nil {
		m.message = fmt.Sprintf("Failed to ship PR #%d: %v", msg.number, msg.err)
	} else {
		m.message = fmt.Sprintf("Shipping PR #%d in session %s", msg.number, msg.groupID)
	}
	m.refreshSessionsFromRuntime(msg.ref)
}

// shipMerged reports whether the instance's PR probe has seen the PR at url
// merged.
func (m *model) shipMerged(fleetName, instance, url string) bool {
	for _, s := range m.runtime[rtKey(fleetName, instance)].GetPrStatus().GetShips() {
		if s.GetUrl() == url {
			return s.GetState() == fleetgrpc.PrState_PR_STATE_MERGED
		}
	}
	return false
}

// shipSessionLabel renders a ship session row's label: the session icon in the
// row's style, the name in the blue ship gradient (green once the PR merged),
// and the ship icon. A selected row is drawn plainly in the selection style so
// the cursor still reads, keeping the ship icon.
func (m *model) shipSessionLabel(r row, icon, text string, style lipgloss.Style, selected bool) string {
	if selected {
		return style.Render(icon+" "+text) + " " + shipIcon
	}
	from, to := activeTheme.ShipFrom, activeTheme.ShipTo
	if m.shipMerged(r.fleetName, r.instance.Name, r.shipURL) {
		from, to = activeTheme.ShippedFrom, activeTheme.ShippedTo
	}
	return style.Render(icon) + " " + renderGradientBetween(text, from, to) + " " + shipIcon
}

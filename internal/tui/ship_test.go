package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/theme"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// shipModel is cursorOnInlinePR with the fleet's Claude Code agent set to
// claude, and the PR status selected (→/l).
func shipModel(t *testing.T, claude bool, ps *fleetgrpc.PrStatus, sessions ...*fleetgrpc.TmuxSession) (*fleetPage, *model) {
	t.Helper()
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	m := autoTagModel(fp, inst, true, ps)
	m.st.Fleets["alpha"].Settings.ClaudeCodeMount = claude
	if rt := m.runtime[rtKey("alpha", "agent-1")]; rt != nil {
		rt.Sessions = sessions
	}
	m.refreshSessionsFromRuntime(InstanceRef{Fleet: "alpha", Instance: "agent-1"})
	fp.buildRows(m)
	for i, r := range fp.rows {
		if r.prStatusInline {
			fp.cursor = i
			fp.rightSelected = true
			return fp, m
		}
	}
	t.Fatal("no inline-PR row built")
	return nil, nil
}

// stubShipExec captures the script a ship start runs in the instance.
func stubShipExec(t *testing.T) *string {
	t.Helper()
	orig := runInstanceCommand
	t.Cleanup(func() { runInstanceCommand = orig })
	var script string
	runInstanceCommand = func(_, _ string, argv []string) (string, int, error) {
		script = argv[len(argv)-1]
		return "", 0, nil
	}
	return &script
}

func TestShipGroupID(t *testing.T) {
	sess := func(names ...string) []tmuxSession {
		out := make([]tmuxSession, 0, len(names))
		for _, n := range names {
			out = append(out, tmuxSession{Name: n})
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		existing []tmuxSession
		want     string
	}{
		{"free", sess("agent-1~dev"), "ship"},
		{"taken", sess("agent-1~ship"), "ship-2"},
		{"taken by a pane", sess("agent-1~ship~ab12"), "ship-2"},
		{"several taken", sess("agent-1~ship", "agent-1~ship-2"), "ship-3"},
		{"other instance's ship", sess("agent-10~ship"), "ship"},
		{"bare session named ship", sess("ship"), "ship"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shipGroupID("agent-1", tc.existing); got != tc.want {
				t.Fatalf("shipGroupID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShipSelectedPR_NeedsClaude(t *testing.T) {
	script := stubShipExec(t)
	fp, m := shipModel(t, false, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
	if cmd := fp.shipSelectedPR(m); cmd != nil {
		t.Fatal("ship must not start without Claude Code enabled")
	}
	if !strings.Contains(m.message, "Claude Code") {
		t.Errorf("message = %q, want a Claude Code hint", m.message)
	}
	if *script != "" {
		t.Errorf("nothing should run in the instance, ran %q", *script)
	}
}

func TestShipSelectedPR_ClosedPR(t *testing.T) {
	fp, m := shipModel(t, true, &fleetgrpc.PrStatus{
		ClosedCount: 1,
		PrSignal:    fleetgrpc.PrSignal_PR_SIGNAL_PURPLE,
		Prs:         []*fleetgrpc.PrRef{ref(9, "https://x/pull/9", "old")},
	})
	if cmd := fp.shipSelectedPR(m); cmd != nil {
		t.Fatal("a closed/merged PR has nothing to ship")
	}
	if !strings.Contains(m.message, "Nothing to ship") {
		t.Errorf("message = %q", m.message)
	}
}

func TestShipSelectedPR_Single(t *testing.T) {
	script := stubShipExec(t)
	fp, m := shipModel(t, true, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
	cmd := fp.shipSelectedPR(m)
	if cmd == nil {
		t.Fatal("expected a ship command")
	}
	if fp.mode != viewNormal {
		t.Errorf("one PR must not open the chooser; mode = %v", fp.mode)
	}
	msg, ok := cmd().(shipStartedMsg)
	if !ok || msg.err != nil || msg.groupID != "ship" || msg.number != 42 {
		t.Fatalf("ship result = %+v", msg)
	}
	for _, want := range []string{
		"tmux new-session -d -s 'agent-1~ship'",
		"tmux set-option -t '=agent-1~ship:' @fleet_ship 'https://x/pull/42'",
		"tmux run-shell -b ",
		"tmux send-keys -t '=agent-1~ship:' -l -- 'claude'",
	} {
		if !strings.Contains(*script, want) {
			t.Errorf("ship script missing %q:\n%s", want, *script)
		}
	}
	m.handleShipStarted(msg)
	if !strings.Contains(m.message, "Shipping PR #42 in session ship") {
		t.Errorf("message = %q", m.message)
	}
}

func TestShipSelectedPR_MultipleOpensShipChooser(t *testing.T) {
	script := stubShipExec(t)
	fp, m := shipModel(t, true, prStatusWithRefs(
		ref(1, "https://x/pull/1", "a"), ref(2, "https://x/pull/2", "b")))
	if cmd := fp.shipSelectedPR(m); cmd != nil {
		t.Fatal("several PRs should open the chooser first")
	}
	if fp.mode != viewChoosePR || !fp.choosePR.ship {
		t.Fatalf("expected the ship chooser, mode = %v ship = %v", fp.mode, fp.choosePR.ship)
	}
	if view := fp.renderChoosePRDialog(m); !strings.Contains(view, "Ship which PR?") || !strings.Contains(view, "enter ship") {
		t.Errorf("chooser should read as a ship chooser:\n%s", view)
	}
	fp.updateChoosePR(m, tea.KeyMsg{Type: tea.KeyDown})
	cmd := fp.updateChoosePR(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter in the ship chooser should ship the PR")
	}
	if msg := cmd().(shipStartedMsg); msg.number != 2 {
		t.Fatalf("shipped PR #%d, want #2", msg.number)
	}
	if !strings.Contains(*script, "@fleet_ship 'https://x/pull/2'") {
		t.Errorf("shipped the wrong PR:\n%s", *script)
	}
	if fp.mode != viewNormal {
		t.Errorf("chooser should close; mode = %v", fp.mode)
	}
}

func TestOpenChooserStaysAnOpenChooser(t *testing.T) {
	fp, m := shipModel(t, true, prStatusWithRefs(
		ref(1, "https://x/pull/1", "a"), ref(2, "https://x/pull/2", "b")))
	fp.choosePR.ship = true // left over from an earlier ship
	fp.openSelectedPR(m)
	if fp.choosePR.ship {
		t.Fatal("enter on the PR status must open the open-PR chooser, not the ship one")
	}
}

func TestShipSelectedPR_AlreadyShipping(t *testing.T) {
	script := stubShipExec(t)
	fp, m := shipModel(t, true, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")),
		&fleetgrpc.TmuxSession{Name: "agent-1~ship", Windows: 1, ShipPrUrl: "https://x/pull/42"})
	if cmd := fp.shipSelectedPR(m); cmd != nil {
		t.Fatal("a PR already shipping must not get a second ship session")
	}
	if !strings.Contains(m.message, "already shipping in ship") {
		t.Errorf("message = %q", m.message)
	}
	if *script != "" {
		t.Errorf("nothing should run in the instance, ran %q", *script)
	}
}

func TestShipSelectedPR_SecondShipTakesNextName(t *testing.T) {
	stubShipExec(t)
	fp, m := shipModel(t, true, prStatusWithRefs(ref(43, "https://x/pull/43", "next")),
		&fleetgrpc.TmuxSession{Name: "agent-1~ship", Windows: 1, ShipPrUrl: "https://x/pull/42"})
	cmd := fp.shipSelectedPR(m)
	if cmd == nil {
		t.Fatal("expected a ship command")
	}
	if msg := cmd().(shipStartedMsg); msg.groupID != "ship-2" {
		t.Fatalf("groupID = %q, want ship-2", msg.groupID)
	}
}

// s ships only while the PR status is selected; on the bare row it keeps its
// old meaning (start/stop, which needs an instance row).
func TestSKeyShipsOnlyWithPRStatusSelected(t *testing.T) {
	stubShipExec(t)
	fp, m := shipModel(t, true, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
	if cmd := fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd == nil {
		t.Fatal("s with the PR status selected should ship")
	}
	fp.rightSelected = false
	if cmd := fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd != nil {
		t.Fatal("s on the bare session row must not ship")
	}
}

func TestShipHelpHint(t *testing.T) {
	fp, m := shipModel(t, true, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
	if help := strings.Join(fp.contextualHelpKeys(m), " | "); !strings.Contains(help, "s: ship") {
		t.Errorf("help should offer s: ship: %s", help)
	}
	fp, m = shipModel(t, false, prStatusWithRefs(ref(42, "https://x/pull/42", "fix")))
	if help := strings.Join(fp.contextualHelpKeys(m), " | "); strings.Contains(help, "s: ship") {
		t.Errorf("help must not offer s: ship without Claude Code: %s", help)
	}
}

// A ship session row carries the ship icon and wears the blue ship gradient,
// switching to the green one once the probe sees its PR merged.
func TestShipSessionRowRendering(t *testing.T) {
	resetTheme(t)
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	const url = "https://x/pull/42"
	ps := prStatusWithRefs(ref(42, url, "fix"))
	ship := &fleetgrpc.TmuxSession{Name: "agent-1~ship", Windows: 1, ShipPrUrl: url}
	plain := &fleetgrpc.TmuxSession{Name: "agent-1~dev", Windows: 1}
	fp, m := shipModel(t, true, ps, plain, ship)
	fp.cursor, fp.rightSelected = 0, false // park the cursor on the fleet header

	rowFor := func(group string) row {
		for _, r := range fp.rows {
			if r.kind == rowSession && r.groupID == group {
				return r
			}
		}
		t.Fatalf("no %s row", group)
		return row{}
	}
	if rowFor("ship").shipURL != url || rowFor("dev").shipURL != "" {
		t.Fatalf("shipURL not threaded onto the rows: ship=%q dev=%q", rowFor("ship").shipURL, rowFor("dev").shipURL)
	}

	view := fp.viewFleetList(m)
	shipLine, devLine := "", ""
	for _, line := range strings.Split(view, "\n") {
		switch {
		case strings.Contains(line, shipIcon):
			shipLine = line
		case strings.Contains(ansi.Strip(line), "○ dev"):
			devLine = line
		}
	}
	if shipLine == "" || !strings.Contains(ansi.Strip(shipLine), "ship "+shipIcon) {
		t.Fatalf("ship row should read \"ship %s\":\n%s", shipIcon, view)
	}
	if devLine == "" || strings.Contains(devLine, shipIcon) {
		t.Fatalf("an ordinary session must not wear the ship icon:\n%s", view)
	}

	r := rowFor("ship")
	shipping := m.shipSessionLabel(r, "○", "ship", sessionStyle, false)
	if !strings.Contains(shipping, gradientStart(activeTheme.ShipFrom)) {
		t.Errorf("shipping label should start in the blue ship gradient: %q", shipping)
	}
	if !strings.Contains(shipLine, gradientStart(activeTheme.ShipFrom)) {
		t.Errorf("the rendered row should wear the ship gradient: %q", shipLine)
	}

	ps.Ships = []*fleetgrpc.PrRef{{Number: 42, Url: url, State: fleetgrpc.PrState_PR_STATE_MERGED}}
	shipped := m.shipSessionLabel(r, "○", "ship", sessionStyle, false)
	if !strings.Contains(shipped, gradientStart(activeTheme.ShippedFrom)) {
		t.Errorf("a merged ship should switch to the green gradient: %q", shipped)
	}

	// Closed without merging: still the shipping colours, not green.
	ps.Ships[0].State = fleetgrpc.PrState_PR_STATE_CLOSED
	if m.shipMerged("alpha", "agent-1", url) {
		t.Error("a PR closed without merging is not shipped")
	}
}

// gradientStart is the truecolor foreground a gradient's first character
// carries (the from endpoint exactly).
func gradientStart(c theme.RGB) string {
	return fmt.Sprintf("38;2;%d;%d;%dm", int(c.R), int(c.G), int(c.B))
}

// shipScreens are captured panes: Claude Code's prompt box (current UI, with a
// placeholder, and the older boxed UI) next to look-alikes that are not it.
var shipScreens = []struct {
	name  string
	pane  string
	ready bool
}{
	{"prompt box", "fleet-man/inst\n" + strings.Repeat("─", 60) + "\n❯\u00a0\n" + strings.Repeat("─", 60) + "\n  ⏵⏵ auto mode on (shift+tab to cycle)\n", true},
	{"prompt box with a placeholder", "\n" + strings.Repeat("─", 80) + "\n❯ Try \"fix lint errors\"\n" + strings.Repeat("─", 80) + "\n  ? for shortcuts\n", true},
	{"older boxed prompt", "╭" + strings.Repeat("─", 58) + "╮\n│ > Try \"refactor <filepath>\"" + strings.Repeat(" ", 24) + "│\n╰" + strings.Repeat("─", 58) + "╯\n", true},
	{"folder-trust prompt", strings.Repeat("─", 60) + "\n Do you trust the files in this folder?\n\n ❯ 1. Yes, proceed\n   2. No, exit\n\n Enter to confirm · Esc to exit\n", false},
	{"shell prompt drawn as ❯", "~/src/fleet-man on main\n❯ \n", false},
	{"a rule above a ❯ only", strings.Repeat("─", 60) + "\n❯ \n", false},
	{"a short rule", strings.Repeat("─", 8) + "\n❯ \n" + strings.Repeat("─", 8) + "\n", false},
	{"booting", "Welcome to Claude Code\n", false},
}

// The prompt-box check reads the same under every awk an image may ship, in a
// byte (C) locale and a UTF-8 one.
func TestShipPromptReadyAWK(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("instances are Linux")
	}
	var awks [][]string
	for _, a := range []string{"awk", "mawk", "gawk", "original-awk"} {
		if _, err := exec.LookPath(a); err == nil {
			awks = append(awks, []string{a})
		}
	}
	if _, err := exec.LookPath("busybox"); err == nil {
		awks = append(awks, []string{"busybox", "awk"})
	}
	if len(awks) == 0 {
		t.Skip("no awk")
	}
	for _, awk := range awks {
		for _, locale := range []string{"C", "C.UTF-8"} {
			for _, sc := range shipScreens {
				c := exec.Command(awk[0], append(awk[1:], shipPromptReadyAWK)...)
				c.Env = append(os.Environ(), "LC_ALL="+locale)
				c.Stdin = strings.NewReader(sc.pane)
				err := c.Run()
				var exitErr *exec.ExitError
				if err != nil && !errors.As(err, &exitErr) {
					t.Fatalf("%v: %v", awk, err)
				}
				if got := err == nil; got != sc.ready {
					t.Errorf("%v (LC_ALL=%s) on %q: ready=%v, want %v", awk, locale, sc.name, got, sc.ready)
				}
			}
		}
	}
}

// The ship script, run in a real tmux, starts Claude in the session and types
// /ship <url> into its prompt box once that shows: never into the
// folder-trust prompt before it, and with Enter as its own keystroke.
func TestShipSessionScriptInRealTmux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("instances are Linux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	sock, err := os.MkdirTemp("", "shp") // short: tmux socket paths are length-capped
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	bin, home, work := t.TempDir(), t.TempDir(), t.TempDir()
	gate, early, typed := filepath.Join(work, "gate"), filepath.Join(work, "early"), filepath.Join(work, "typed")
	rule := strings.Repeat("─", 40)
	// A fake Claude: run bare, it shows a folder-trust prompt until the test
	// opens the gate, keeps whatever was typed meanwhile, then draws its prompt
	// box and keeps the line typed into it.
	stub := "#!/bin/sh\n" +
		"[ \"$#\" -eq 0 ] || exit 0\n" +
		"printf 'Do you trust the files in this folder?\\n\\n❯ 1. Yes, proceed\\n  2. No, exit\\n'\n" +
		"while [ ! -e " + shQuote(gate) + " ]; do sleep 0.1; done\n" +
		"stty -icanon min 0 time 0; dd bs=4096 count=1 > " + shQuote(early) + " 2>/dev/null; stty icanon\n" +
		"printf '%s\\n❯\\302\\240\\n%s\\n' " + shQuote(rule) + " " + shQuote(rule) + "\n" +
		"IFS= read -r line; printf '%s\\n' \"$line\" > " + shQuote(typed) + "\n" +
		"exec sleep 600\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"SHELL=/bin/sh",
		"TMUX_TMPDIR=" + sock,
		"TERM=xterm",
	}
	run := func(script string) (string, error) {
		c := exec.Command("sh", "-c", script)
		c.Env = env // no TMUX: never the caller's server
		out, err := c.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = run("tmux kill-server 2>/dev/null") })
	pane := func() string { out, _ := run("tmux capture-pane -p -t '=inst~ship:'"); return out }
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened; pane:\n%s", what, pane())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	const url = "https://github.com/o/r/pull/7"
	if out, err := run(buildShipSessionScript("inst~ship", url)); err != nil {
		t.Fatalf("ship script failed: %v\n%s", err, out)
	}
	out, err := run(backend.ListSessionsScript)
	if err != nil {
		t.Fatalf("list sessions: %v\n%s", err, out)
	}
	if !strings.Contains(out, "inst~ship:1:0:"+url) {
		t.Fatalf("session list %q should report the ship URL", out)
	}

	waitFor("claude starting in the session", func() bool { return strings.Contains(pane(), "Yes, proceed") })
	time.Sleep(2500 * time.Millisecond) // a few of the waiter's polls
	if _, err := os.Stat(typed); err == nil {
		t.Fatal("a line was typed before Claude's prompt box showed")
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("a line typed into the prompt box", func() bool { _, err := os.Stat(typed); return err == nil })
	got, _ := os.ReadFile(typed)
	if strings.TrimSpace(string(got)) != shipPromptPrefix+url {
		t.Fatalf("typed %q into the prompt box, want %q", got, shipPromptPrefix+url)
	}
	if e, _ := os.ReadFile(early); len(e) != 0 {
		t.Fatalf("typed %q while the folder-trust prompt showed", e)
	}

	// A second ship under the same name leaves the first session alone.
	if out, err := run(buildShipSessionScript("inst~ship", "https://github.com/o/r/pull/8")); err == nil {
		t.Fatalf("a duplicate ship session should fail, got:\n%s", out)
	}
	out, _ = run(backend.ListSessionsScript)
	if !strings.Contains(out, "inst~ship:1:0:"+url) {
		t.Fatalf("a failed duplicate ship must not touch the existing session: %q", out)
	}
}

// The waiter gives up as soon as its session is gone.
func TestShipPromptWaiterStopsWithItsSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("instances are Linux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	sock, err := os.MkdirTemp("", "shp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + t.TempDir(), "SHELL=/bin/sh", "TMUX_TMPDIR=" + sock, "TERM=xterm"}
	run := func(script string) (string, error) {
		c := exec.Command("sh", "-c", script)
		c.Env = env
		out, err := c.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = run("tmux kill-server 2>/dev/null") })
	// A keeper session holds the server up once the ship session is gone.
	if out, err := run("tmux new-session -d -s keep"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(buildShipSessionScript("stopw~ship", "https://github.com/o/r/pull/7")); err != nil {
		t.Fatalf("ship script failed: %v\n%s", err, out)
	}
	waiting := func() bool {
		out, _ := run("ps -eww -o args")
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "=stopw~ship") && strings.Contains(l, "capture-pane") {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for !waiting() {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, err := run("tmux kill-session -t '=stopw~ship'"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	deadline = time.Now().Add(10 * time.Second) // the waiter checks once a second
	for waiting() {
		if time.Now().After(deadline) {
			out, _ := run("ps -eww -o pid,ppid,stat,args; tmux ls")
			t.Fatalf("the waiter kept running after its session was killed:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

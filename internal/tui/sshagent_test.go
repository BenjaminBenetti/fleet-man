package tui

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// clearArmadaEnv points the TUI at the local daemon for the test (and drops
// an agent hint inherited from a TUI-spawned pane).
func clearArmadaEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{fleetclient.EnvGateway, fleetclient.EnvSSH, fleetclient.EnvServer, fleetclient.EnvToken, fleetclient.EnvTUIProvidesAgent} {
		t.Setenv(key, "")
	}
}

func TestArmadaAgentToggleSavesAndKeepsTheCursor(t *testing.T) {
	clearArmadaEnv(t)
	var saved []configutil.ArmadaRemote
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) ([]configutil.ArmadaRemote, error) {
		saved = remotes
		return remotes, nil
	}
	defer func() { saveArmadaLocal = origSave }()

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "ssh://ben@devbox"},
		{URL: "https://gw.example.com/abc", Token: "t"},
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)

	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	if !sp.armadaAgentFocused || sp.armadaDeleteFocused {
		t.Fatal("right should focus the agent toggle first")
	}
	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the agent toggle should save")
	}
	m.handleArmadaMsg(cmd().(armadaSaveResultMsg))

	if len(saved) != 2 || !saved[0].ForwardAgent || saved[1].ForwardAgent {
		t.Fatalf("persisted registry = %+v, want forwarding on for the first remote only", saved)
	}
	if !m.armadaRemotes[0].ForwardAgent {
		t.Fatal("the model should adopt the saved registry")
	}
	if !sp.armadaAgentFocused || sp.settingsCursorItem(m) != settingsItemArmadaBase {
		t.Fatal("the cursor should stay on the toggled row's agent toggle")
	}
	if !strings.Contains(m.message, "SSH agent forwarding on") {
		t.Fatalf("status message = %q", m.message)
	}

	// And back off.
	cmd = sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.handleArmadaMsg(cmd().(armadaSaveResultMsg))
	if saved[0].ForwardAgent || !strings.Contains(m.message, "forwarding off") {
		t.Fatalf("second toggle: saved %+v, message %q", saved, m.message)
	}
}

func TestArmadaRemoteRowSubCursorWalksBothWays(t *testing.T) {
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://ben@devbox"}}
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)

	right := func() { sp.Update(m, tea.KeyMsg{Type: tea.KeyRight}) }
	left := func() { sp.Update(m, tea.KeyMsg{Type: tea.KeyLeft}) }
	at := func(agent, del bool) {
		t.Helper()
		if sp.armadaAgentFocused != agent || sp.armadaDeleteFocused != del {
			t.Fatalf("sub-cursor agent=%v delete=%v, want agent=%v delete=%v", sp.armadaAgentFocused, sp.armadaDeleteFocused, agent, del)
		}
	}
	right()
	at(true, false)
	right()
	at(false, true)
	right() // stays on delete
	at(false, true)
	left()
	at(true, false)
	left()
	at(false, false)
}

func TestAgentForwardTargetFollowsTheConnectionAndTheToggle(t *testing.T) {
	clearArmadaEnv(t)
	m := armadaTestModel(nil)
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "ssh://ben@devbox", ForwardAgent: true},
		{URL: "ssh://ben@other"},
		{URL: "https://gw.example.com/abc", Token: "t", ForwardAgent: true},
	}

	if got := m.agentForwardTarget(); got != "" {
		t.Fatalf("local connection: target = %q, want none", got)
	}
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	if got := m.agentForwardTarget(); got != "ssh://ben@devbox" {
		t.Fatalf("connected to devbox: target = %q", got)
	}
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@other")
	if got := m.agentForwardTarget(); got != "" {
		t.Fatalf("forwarding off for other: target = %q", got)
	}
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvGateway, "https://gw.example.com/abc")
	if got := m.agentForwardTarget(); got != "https://gw.example.com/abc" {
		t.Fatalf("gateway remote with forwarding on: target = %q", got)
	}
	t.Setenv(fleetclient.EnvGateway, "https://unregistered.example.com/x")
	if got := m.agentForwardTarget(); got != "" {
		t.Fatalf("an unregistered remote has no toggle: target = %q", got)
	}
}

// stubAgentProvider replaces the provider goroutine with one that records its
// start and waits for cancellation.
func stubAgentProvider(t *testing.T) (starts func() int, running func() int) {
	t.Helper()
	var mu sync.Mutex
	started, live := 0, 0
	orig := runAgentProviderFn
	runAgentProviderFn = func(ctx context.Context, _ *tea.Program, gen int, _ string) {
		mu.Lock()
		started++
		live++
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		live--
		mu.Unlock()
		agentProviderExited(gen)
	}
	ctx, cancel := context.WithCancel(context.Background())
	startAgentControl(ctx, nil)
	t.Cleanup(func() {
		cancel()
		runAgentProviderFn = orig
		agentCtl.mu.Lock()
		agentCtl.parent, agentCtl.program, agentCtl.cancel, agentCtl.target = nil, nil, nil, ""
		agentCtl.mu.Unlock()
	})
	return func() int { mu.Lock(); defer mu.Unlock(); return started },
		func() int { mu.Lock(); defer mu.Unlock(); return live }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSyncAgentProviderStartsStopsAndRetargets(t *testing.T) {
	clearArmadaEnv(t)
	starts, running := stubAgentProvider(t)
	m := armadaTestModel(nil)
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "ssh://ben@devbox", ForwardAgent: true},
		{URL: "ssh://ben@other", ForwardAgent: true},
	}

	m.syncAgentProvider() // local: nothing to provide to
	if starts() != 0 {
		t.Fatal("no provider should start while connected locally")
	}

	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	m.syncAgentProvider()
	m.syncAgentProvider() // idempotent
	waitFor(t, "the provider to start", func() bool { return running() == 1 })
	if starts() != 1 {
		t.Fatalf("starts = %d, want 1", starts())
	}

	t.Setenv(fleetclient.EnvSSH, "ssh://ben@other") // an armada switch
	m.syncAgentProvider()
	waitFor(t, "the provider to move to the new remote", func() bool { return starts() == 2 && running() == 1 })

	m.armadaRemotes[1].ForwardAgent = false // toggled off
	m.syncAgentProvider()
	waitFor(t, "the provider to stop", func() bool { return running() == 0 })
}

func TestArmadaAgentStatusValue(t *testing.T) {
	clearArmadaEnv(t)
	m := armadaTestModel(nil)
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")

	if got := armadaAgentStatusValue(m, "ssh://ben@devbox", false); got != "" {
		t.Fatalf("forwarding off: %q", got)
	}
	if got := armadaAgentStatusValue(m, "ssh://ben@elsewhere", true); got != "" {
		t.Fatalf("not the current connection: %q", got)
	}
	cases := []struct {
		status agentfwd.Status
		want   string
	}{
		{agentfwd.Status{State: agentfwd.StateActive, Uses: 3}, "used 3 times"},
		{agentfwd.Status{State: agentfwd.StateActive}, "no uses yet"},
		{agentfwd.Status{State: agentfwd.StateStandby}, "standing by — a newer client is attached"},
		{agentfwd.Status{State: agentfwd.StateStandby, Uses: 2}, "used 2 times"},
		{agentfwd.Status{State: agentfwd.StateStandby, Uses: 1}, "used once"},
		{agentfwd.Status{State: agentfwd.StateNoAgent, Detail: "SSH_AUTH_SOCK is not set"}, "SSH_AUTH_SOCK is not set"},
		{agentfwd.Status{State: agentfwd.StateRefused, Detail: "turned off on this host"}, "turned off on this host"},
		{agentfwd.Status{State: agentfwd.StateUnsupported}, "too old"},
		{agentfwd.Status{State: agentfwd.StateConnecting}, "connecting"},
	}
	for _, c := range cases {
		m.agentStatus = c.status
		if got := armadaAgentStatusValue(m, "ssh://ben@devbox", true); !strings.Contains(got, c.want) {
			t.Errorf("state %v: %q does not mention %q", c.status.State, got, c.want)
		}
	}
	m.agentStatus = agentfwd.Status{State: agentfwd.StateStandby}
	if got := armadaAgentStatusValue(m, "ssh://ben@devbox", true); strings.Contains(got, "uses") {
		t.Errorf("an unused standby provider shows no count: %q", got)
	}
}

// agentTarget is the remote the running provider serves ("" when none).
func agentTarget() string {
	agentCtl.mu.Lock()
	defer agentCtl.mu.Unlock()
	return agentCtl.target
}

// armadaEntryFor finds the Armada selector entry for url.
func armadaEntryFor(t *testing.T, m *model, url string) armadaEntry {
	t.Helper()
	for _, e := range m.armadaEntries() {
		if e.url == url {
			return e
		}
	}
	t.Fatalf("no armada entry for %s", url)
	return armadaEntry{}
}

// TestAgentProviderStartsWhenTheRegistryLoads: a TUI booted connected to a
// remote starts providing as soon as the registry says forwarding is on.
func TestAgentProviderStartsWhenTheRegistryLoads(t *testing.T) {
	clearArmadaEnv(t)
	_, running := stubAgentProvider(t)
	m := armadaTestModel(nil)
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")

	m.handleArmadaMsg(armadaLoadedMsg{remotes: []configutil.ArmadaRemote{
		{URL: "ssh://ben@devbox", ForwardAgent: true},
		{URL: "ssh://ben@other"},
	}})
	waitFor(t, "the provider to start", func() bool { return running() == 1 })
	if got := agentTarget(); got != "ssh://ben@devbox" {
		t.Fatalf("provider target = %q", got)
	}
}

// TestAgentProviderFollowsTheRegistryRecheck: [ agent: on ] turned off
// elsewhere (another TUI) stops this TUI's provider on the next re-read, and a
// re-read that fails keeps it running; the recheck always re-arms.
func TestAgentProviderFollowsTheRegistryRecheck(t *testing.T) {
	clearArmadaEnv(t)
	_, running := stubAgentProvider(t)
	m := armadaTestModel(nil)
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	m.handleArmadaMsg(armadaLoadedMsg{remotes: []configutil.ArmadaRemote{{URL: "ssh://ben@devbox", ForwardAgent: true}}})
	waitFor(t, "the provider to start", func() bool { return running() == 1 })

	if cmd := m.handleArmadaMsg(armadaRecheckMsg{err: context.DeadlineExceeded}); cmd == nil {
		t.Fatal("a failed recheck must re-arm")
	}
	if running() != 1 {
		t.Fatal("an unreadable registry must not stop the provider")
	}
	if cmd := m.handleArmadaMsg(armadaRecheckMsg{remotes: []configutil.ArmadaRemote{{URL: "ssh://ben@devbox"}}}); cmd == nil {
		t.Fatal("the recheck must re-arm")
	}
	waitFor(t, "the provider to stop", func() bool { return running() == 0 })
}

// TestAgentProviderFollowsArmadaSwitches: switching to a remote with
// forwarding off stops the provider; switching to one with it on starts it or
// moves it there.
func TestAgentProviderFollowsArmadaSwitches(t *testing.T) {
	clearArmadaEnv(t)
	starts, running := stubAgentProvider(t)
	m := armadaTestModel(nil)
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "ssh://ben@devbox", ForwardAgent: true},
		{URL: "ssh://ben@other"},
		{URL: "https://gw.example.com/abc", Token: "t", ForwardAgent: true},
	}
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	m.syncAgentProvider()
	waitFor(t, "the provider to start", func() bool { return running() == 1 })

	m.switchArmada(armadaEntryFor(t, m, "ssh://ben@other"))
	waitFor(t, "the provider to stop", func() bool { return running() == 0 })
	if got := agentTarget(); got != "" {
		t.Fatalf("provider target after switching to a remote with forwarding off = %q", got)
	}

	m.switchArmada(armadaEntryFor(t, m, "ssh://ben@devbox"))
	waitFor(t, "the provider to start again", func() bool { return starts() == 2 && running() == 1 })

	m.switchArmada(armadaEntryFor(t, m, "https://gw.example.com/abc"))
	waitFor(t, "the provider to move", func() bool { return starts() == 3 && running() == 1 })
	if got := agentTarget(); got != "https://gw.example.com/abc" {
		t.Fatalf("provider target = %q, want the gateway remote", got)
	}

	m.switchArmada(armadaEntryFor(t, m, "")) // local
	waitFor(t, "the provider to stop", func() bool { return running() == 0 })
}

// TestArmadaAgentToggleWhileConnectedStartsTheProvider: turning forwarding on
// for the remote the TUI is on starts providing at once (through the save
// result), and turning it off stops it.
func TestArmadaAgentToggleWhileConnectedStartsTheProvider(t *testing.T) {
	clearArmadaEnv(t)
	_, running := stubAgentProvider(t)
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) ([]configutil.ArmadaRemote, error) {
		return remotes, nil
	}
	t.Cleanup(func() { saveArmadaLocal = origSave })

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://ben@devbox"}, {URL: "ssh://ben@other"}}
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	m.syncAgentProvider()
	if running() != 0 {
		t.Fatal("no provider before the toggle")
	}

	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	m.handleArmadaMsg(sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})().(armadaSaveResultMsg))
	waitFor(t, "the provider to start", func() bool { return running() == 1 })
	if got := agentTarget(); got != "ssh://ben@devbox" {
		t.Fatalf("provider target = %q", got)
	}
	if m.message != "SSH agent forwarding on for devbox" {
		t.Fatalf("status message = %q", m.message)
	}

	m.handleArmadaMsg(sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})().(armadaSaveResultMsg))
	waitFor(t, "the provider to stop", func() bool { return running() == 0 })
}

// TestArmadaAdoptsTheSavedForwardAgent: the daemon turns forwarding on for a
// new ssh:// remote whose ssh config forwards the agent. The TUI adopts the
// list as saved, so the toggle shows it and a later save of the registry (a
// delete of another row) keeps it on instead of silently clearing it.
func TestArmadaAdoptsTheSavedForwardAgent(t *testing.T) {
	clearArmadaEnv(t)
	origPing := pingArmadaRemote
	pingArmadaRemote = func(string, string) error { return nil }
	t.Cleanup(func() { pingArmadaRemote = origPing })
	var sent [][]configutil.ArmadaRemote
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) ([]configutil.ArmadaRemote, error) {
		sent = append(sent, slices.Clone(remotes))
		saved := slices.Clone(remotes)
		if len(sent) == 1 {
			// The add: ForwardAgent inherited from the user's ssh config.
			for i := range saved {
				if saved[i].URL == "ssh://ben@desktop" {
					saved[i].ForwardAgent = true
				}
			}
		}
		return saved, nil
	}
	t.Cleanup(func() { saveArmadaLocal = origSave })

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "https://gw.example.com/abc", Token: "t"}}

	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaAdd)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	typeRunes(sp, m, "ssh://ben@desktop")
	testMsg := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})().(armadaTestResultMsg)
	m.handleArmadaMsg(m.handleArmadaMsg(testMsg)())

	if len(sent) != 1 || sent[0][1].ForwardAgent {
		t.Fatalf("add sent %+v: the TUI itself does not turn forwarding on", sent)
	}
	if len(m.armadaRemotes) != 2 || m.armadaRemotes[1].URL != "ssh://ben@desktop" || !m.armadaRemotes[1].ForwardAgent {
		t.Fatalf("model registry = %+v, want the saved ForwardAgent adopted", m.armadaRemotes)
	}
	if view := sp.viewSettings(m); !strings.Contains(view, "[ agent: on ]") {
		t.Fatal("the adopted remote's row should show [ agent: on ]")
	}
	if !strings.Contains(m.message, "from your ssh config") {
		t.Fatalf("status line %q should say forwarding came on from the ssh config", m.message)
	}

	// Delete the OTHER row (the gateway remote).
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight}) // [ agent ]
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight}) // [ delete ]
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}) // arm
	m.handleArmadaMsg(sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})().(armadaSaveResultMsg))

	if len(sent) != 2 {
		t.Fatalf("saves = %d, want the add and the delete", len(sent))
	}
	want := []configutil.ArmadaRemote{{URL: "ssh://ben@desktop", ForwardAgent: true}}
	if !slices.Equal(sent[1], want) {
		t.Fatalf("delete sent %+v, want %+v", sent[1], want)
	}
}

// TestAttachExecCmdReinvokesFleetShell: a remote attach re-invokes this
// binary's `fleet shell` with this process's environment (the connection)
// plus the TUI's agent hint, so the child's yielding provider neither delays
// the shell nor prints a notice this TUI already shows. A local attach is the
// server-resolved argv and carries no hint.
func TestAttachExecCmdReinvokesFleetShell(t *testing.T) {
	clearArmadaEnv(t)
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	cmd, err := attachExecCmd("alpha", "inst", []string{"bash"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args[1:], []string{"shell", "alpha/inst", "--", "bash"}) {
		t.Fatalf("args = %v", cmd.Args)
	}
	if got := effectiveEnv(cmd, fleetclient.EnvSSH); got != "ssh://ben@devbox" {
		t.Fatalf("child %s = %q, want the TUI's connection", fleetclient.EnvSSH, got)
	}
	if got := effectiveEnv(cmd, fleetclient.EnvTUIProvidesAgent); got != "1" {
		t.Fatalf("child %s = %q, want the TUI's agent hint", fleetclient.EnvTUIProvidesAgent, got)
	}

	t.Setenv(fleetclient.EnvSSH, "")
	origResolve := resolveExecArgv
	t.Cleanup(func() { resolveExecArgv = origResolve })
	resolveExecArgv = func(string, string, []string) ([]string, map[string]string, error) {
		return []string{"docker", "exec", "-it", "c1", "bash"}, nil, nil
	}
	cmd, err = attachExecCmd("alpha", "inst", []string{"bash"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "docker" {
		t.Fatalf("local attach args = %v, want the resolved argv", cmd.Args)
	}
	if got := effectiveEnv(cmd, fleetclient.EnvTUIProvidesAgent); got != "" {
		t.Fatalf("local attach %s = %q, want no hint", fleetclient.EnvTUIProvidesAgent, got)
	}
}

// effectiveEnv is the value of key the child of cmd sees: from cmd.Env (the
// last entry wins, as os/exec does) or, when that is nil, this process's.
func effectiveEnv(cmd *exec.Cmd, key string) string {
	if cmd.Env == nil {
		return os.Getenv(key)
	}
	value := ""
	for _, kv := range cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}
	return value
}

func TestExecWithBannerKeepsTheCommandsEnvironment(t *testing.T) {
	cmd := exec.Command("fleet", "shell", "f/i")
	cmd.Env = []string{"PATH=/bin", fleetclient.EnvTUIProvidesAgent + "=1"}
	wrapped := execWithBannerCmd("banner", cmd)
	if !slices.Equal(wrapped.Env, cmd.Env) {
		t.Fatalf("the banner wrapper dropped the environment: %v", wrapped.Env)
	}
}

func TestAgentProviderNeverDialsAnotherRemote(t *testing.T) {
	clearArmadaEnv(t)
	// An Armada switch has already rewritten the env to another remote when
	// the provider started for devbox wakes up: it must stop, not dial.
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@other")
	done := make(chan struct{})
	go func() {
		runAgentProvider(context.Background(), nil, -1, "ssh://ben@devbox")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider dialed (or kept retrying) a remote it was not started for")
	}
}

// TestArmadaAgentToggleKeepsTheRowInView: toggling [ agent ] adds a status
// message below the viewport (and may grow the row) without moving the
// cursor; a row at the viewport's bottom edge must stay visible. A wheel
// scroll that changes nothing about the selection is still left alone.
func TestArmadaAgentToggleKeepsTheRowInView(t *testing.T) {
	clearArmadaEnv(t)
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) ([]configutil.ArmadaRemote, error) { return remotes, nil }
	defer func() { saveArmadaLocal = origSave }()

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.height = 24
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://ben@devbox"}}
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	inView := func(when string) {
		t.Helper()
		c := sp.lastChase
		if c.start < sp.scrollOffset || c.start+c.height > sp.scrollOffset+c.viewHeight {
			t.Fatalf("%s: row lines %d..%d outside the viewport %d..%d", when, c.start, c.start+c.height-1, sp.scrollOffset, sp.scrollOffset+c.viewHeight-1)
		}
	}
	sp.viewSettings(m)
	inView("before the toggle")
	if sp.scrollOffset == 0 {
		t.Fatal("the test needs the row below the first screenful")
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.handleArmadaMsg(cmd().(armadaSaveResultMsg))
	if m.message == "" {
		t.Fatal("the toggle should leave a status message")
	}
	sp.viewSettings(m)
	inView("after the toggle")

	sp.scrollOffset = 0 // a wheel scroll to the top
	sp.viewSettings(m)
	if sp.scrollOffset != 0 {
		t.Fatalf("a plain re-render yanked a wheel scroll back to offset %d", sp.scrollOffset)
	}
}

// TestSettingsPingSweepLeavesAWheelScrollAlone: the ping sweep re-wraps an
// erroring remote's row (pinging… ↔ error <text>) every few seconds; that
// must not yank a viewport the user wheel-scrolled away back to the cursor.
func TestSettingsPingSweepLeavesAWheelScrollAlone(t *testing.T) {
	clearArmadaEnv(t)
	for _, width := range []int{80, 100, 120, 140} {
		sp := newSettingsPage()
		m := armadaTestModel(sp)
		m.width, m.height = width, 24
		const url = "ssh://ben@devbox"
		m.armadaRemotes = []configutil.ArmadaRemote{{URL: url}}
		m.armadaStatus[url] = armadaStatus{state: armadaStatusError, err: "unknown session — daemon offline or Remote Fleet disabled"}
		sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
		sp.viewSettings(m)
		if c := sp.lastChase; c.start+c.height <= c.viewHeight {
			t.Fatalf("width %d: the test needs the row below the first screenful", width)
		}

		sp.scrollOffset = 0 // a wheel scroll to the top
		sp.viewSettings(m)
		m.armadaStatus[url] = armadaStatus{state: armadaStatusPinging}
		sp.viewSettings(m)
		m.armadaStatus[url] = armadaStatus{state: armadaStatusError, err: "unknown session — daemon offline or Remote Fleet disabled"}
		sp.viewSettings(m)
		if sp.scrollOffset != 0 {
			t.Fatalf("width %d: the ping sweep yanked a wheel scroll to offset %d", width, sp.scrollOffset)
		}
	}
}

// TestStatusMessageWrapsToTheTerminal: the renderer cuts lines at the
// terminal width, and a clone failure's hint is its last, longest line.
func TestStatusMessageWrapsToTheTerminal(t *testing.T) {
	hint := "hint: no SSH key on this host is accepted by the git server — on a remote fleet, turn on [ agent: on ] on its row in Settings → Fleet Armada (right arrow, enter) to use your own keys, and keep the TUI connected"
	out := renderMessage("Failed to create app/x: git clone failed\n"+hint, 80)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("line %q is %d cells wide, over the 80-column terminal", line, w)
		}
	}
	if !strings.Contains(strings.Join(strings.Fields(ansi.Strip(out)), " "), "keep the TUI connected") {
		t.Fatalf("the hint's end was lost: %q", out)
	}
}

// TestArmadaRowHelpFitsAnEightyColumnTerminal: the renderer cuts lines at the
// terminal width, so the row's key help — which ends with how to delete —
// must fit a common 80 columns whole.
func TestArmadaRowHelpFitsAnEightyColumnTerminal(t *testing.T) {
	clearArmadaEnv(t)
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.width = 80
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "ssh://ben@devbox"}}
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	for _, line := range strings.Split(sp.viewSettings(m), "\n") {
		if strings.Contains(line, "enter: ping now") {
			if w := lipgloss.Width(line); w > 80 || !strings.Contains(line, "[ delete ] (enter twice)") {
				t.Fatalf("help line (%d cells) = %q", w, ansi.Strip(line))
			}
			return
		}
	}
	t.Fatal("no Armada row help line")
}

package tui

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// clearArmadaEnv points the TUI at the local daemon for the test.
func clearArmadaEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{fleetclient.EnvGateway, fleetclient.EnvSSH, fleetclient.EnvServer, fleetclient.EnvToken} {
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
	runAgentProviderFn = func(ctx context.Context, _ *tea.Program, gen int) {
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

// stubTmuxEnv records what would be mirrored into the tmux server's global
// environment ("" = unset).
func stubTmuxEnv(t *testing.T) map[string]string {
	t.Helper()
	got := make(map[string]string)
	orig := setTmuxGlobalEnv
	setTmuxGlobalEnv = func(name, value string) { got[name] = value }
	t.Cleanup(func() { setTmuxGlobalEnv = orig })
	return got
}

// TestAgentProviderStartsWhenTheRegistryLoads: a TUI booted connected to a
// remote starts providing as soon as the registry says forwarding is on, and
// tells the tmux panes it spawns that it does.
func TestAgentProviderStartsWhenTheRegistryLoads(t *testing.T) {
	clearArmadaEnv(t)
	_, running := stubAgentProvider(t)
	tmuxEnv := stubTmuxEnv(t)
	m := armadaTestModel(nil)
	m.inHostTmux = true
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")

	m.handleArmadaMsg(armadaLoadedMsg{remotes: []configutil.ArmadaRemote{
		{URL: "ssh://ben@devbox", ForwardAgent: true},
		{URL: "ssh://ben@other"},
	}})
	waitFor(t, "the provider to start", func() bool { return running() == 1 })
	if got := agentTarget(); got != "ssh://ben@devbox" {
		t.Fatalf("provider target = %q", got)
	}
	if got := tmuxEnv[fleetclient.EnvAgentProviderPID]; got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("tmux %s = %q, want this TUI's pid", fleetclient.EnvAgentProviderPID, got)
	}
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

// TestAttachExecCmdNamesTheProvidingTUI: a remote attach's `fleet shell`
// child learns this TUI's pid, so it leaves agent forwarding to the TUI.
func TestAttachExecCmdNamesTheProvidingTUI(t *testing.T) {
	clearArmadaEnv(t)
	t.Setenv(fleetclient.EnvSSH, "ssh://ben@devbox")
	cmd, err := attachExecCmd("alpha", "inst", []string{"bash"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args[1:], []string{"shell", "alpha/inst", "--", "bash"}) {
		t.Fatalf("args = %v", cmd.Args)
	}
	want := fleetclient.EnvAgentProviderPID + "=" + strconv.Itoa(os.Getpid())
	if len(cmd.Env) == 0 || cmd.Env[len(cmd.Env)-1] != want {
		t.Fatalf("child env should end with %q (last wins over an inherited one)", want)
	}
	if !slices.Contains(cmd.Env, fleetclient.EnvSSH+"=ssh://ben@devbox") {
		t.Fatal("the child must still inherit the connection env")
	}
}

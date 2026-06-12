package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// armadaTestModel builds a model with the pieces the armada paths touch. The
// width keeps long gateway URLs on one line so substring assertions hold.
func armadaTestModel(sp *settingsPage) *model {
	m := &model{
		config:       state.DefaultConfig(),
		toolStatus:   allToolsFound(),
		spinner:      spinner.New(),
		armadaStatus: make(map[string]armadaStatus),
		runtime:      make(map[string]*fleetgrpc.InstanceRuntime),
		creating:     make(map[string]bool),
		fleetPage:    newFleetPage(),
		portForwards: portforward.NewManager(),
		sessionStore: NewSessionStore(),
		st:           &configutil.State{},
		width:        160,
	}
	if sp != nil {
		m.currentPage = sp
	} else {
		m.currentPage = m.fleetPage
	}
	return m
}

// typeRunes feeds a string into the active page handler one KeyMsg at a time.
func typeRunes(sp *settingsPage, m *model, s string) {
	for _, r := range s {
		sp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestSettingsSectionIncludesFleetArmada confirms the section renders with one
// row per registered remote (before the add button) and the "+ Remote Fleet"
// button, and that the rows show the URL.
func TestSettingsSectionIncludesFleetArmada(t *testing.T) {
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "https://gw.example.com/abc", Token: "t1"},
		{URL: "https://gw2.example.com/def", Token: "t2"},
	}

	items := sp.visibleItems(m)
	posR0 := settingsPositionOf(sp, m, settingsItemArmadaBase)
	posR1 := settingsPositionOf(sp, m, settingsItemArmadaBase+1)
	posAdd := settingsPositionOf(sp, m, settingsItemArmadaAdd)
	if posR0 < 0 || posR1 < 0 || posAdd < 0 {
		t.Fatalf("armada items missing from settings nav: r0=%d r1=%d add=%d (items=%v)", posR0, posR1, posAdd, items)
	}
	if !(posR0 < posR1 && posR1 < posAdd) {
		t.Fatalf("armada rows must precede the add button: r0=%d r1=%d add=%d", posR0, posR1, posAdd)
	}

	out := sp.viewSettings(m)
	if !strings.Contains(out, "Fleet Armada") {
		t.Fatal("settings view missing the Fleet Armada section header")
	}
	if !strings.Contains(out, "https://gw.example.com/abc") || !strings.Contains(out, "https://gw2.example.com/def") {
		t.Fatal("settings view missing registered remote URLs")
	}
	if !strings.Contains(out, "+ Remote Fleet") {
		t.Fatal("settings view missing the + Remote Fleet button")
	}
	if !strings.Contains(out, "[ delete ]") {
		t.Fatal("settings view missing the [ delete ] buttons")
	}
}

// TestArmadaAddFlowRegistersRemote walks the whole "+ Remote Fleet" flow: URL
// entry, token entry (masked), connection test, then persistence — with the
// network + persistence seams stubbed.
func TestArmadaAddFlowRegistersRemote(t *testing.T) {
	pingedURL, pingedToken := "", ""
	origPing := pingArmadaRemote
	pingArmadaRemote = func(url, token string) error {
		pingedURL, pingedToken = url, token
		return nil
	}
	defer func() { pingArmadaRemote = origPing }()

	var saved []configutil.ArmadaRemote
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) error {
		saved = remotes
		return nil
	}
	defer func() { saveArmadaLocal = origSave }()

	sp := newSettingsPage()
	m := armadaTestModel(sp)

	// Enter on the add button starts the URL stage.
	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaAdd)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if sp.armadaAddStage != armadaAddURLIn {
		t.Fatalf("stage = %v, want URL input", sp.armadaAddStage)
	}

	typeRunes(sp, m, "https://gw.example.com/abc")
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if sp.armadaAddStage != armadaAddTokenIn {
		t.Fatalf("stage = %v, want token input", sp.armadaAddStage)
	}

	typeRunes(sp, m, "secret-token")
	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if sp.armadaAddStage != armadaAddTesting {
		t.Fatalf("stage = %v, want testing", sp.armadaAddStage)
	}
	if cmd == nil {
		t.Fatal("token commit should return the connection-test command")
	}

	// Run the test command, then feed its result through the central handler;
	// success chains into the save command.
	testMsg, ok := cmd().(armadaTestResultMsg)
	if !ok {
		t.Fatalf("expected armadaTestResultMsg, got %T", testMsg)
	}
	if pingedURL != "https://gw.example.com/abc" || pingedToken != "secret-token" {
		t.Fatalf("connection test used %s/%s", pingedURL, pingedToken)
	}
	saveCmd := m.handleArmadaMsg(testMsg)
	if saveCmd == nil {
		t.Fatal("successful test should chain into the save command")
	}
	saveMsg, ok := saveCmd().(armadaSaveResultMsg)
	if !ok || saveMsg.err != nil {
		t.Fatalf("save failed: %+v", saveMsg)
	}
	m.handleArmadaMsg(saveMsg)

	if len(saved) != 1 || saved[0].URL != "https://gw.example.com/abc" || saved[0].Token != "secret-token" {
		t.Fatalf("persisted registry mismatch: %+v", saved)
	}
	if len(m.armadaRemotes) != 1 || m.armadaRemotes[0].URL != "https://gw.example.com/abc" {
		t.Fatalf("model registry mismatch: %+v", m.armadaRemotes)
	}
	if sp.armadaAddStage != armadaAddNone {
		t.Fatalf("add flow should be reset, stage = %v", sp.armadaAddStage)
	}
	if m.armadaStatus["https://gw.example.com/abc"].state != armadaStatusConnected {
		t.Fatal("freshly tested remote should show connected")
	}
}

// TestArmadaAddFlowFailedTestDoesNotRegister verifies a failed connection test
// surfaces the cause and leaves the registry untouched.
func TestArmadaAddFlowFailedTestDoesNotRegister(t *testing.T) {
	origPing := pingArmadaRemote
	pingArmadaRemote = func(url, token string) error { return errors.New("boom") }
	defer func() { pingArmadaRemote = origPing }()

	saveCalled := false
	origSave := saveArmadaLocal
	saveArmadaLocal = func([]configutil.ArmadaRemote) error {
		saveCalled = true
		return nil
	}
	defer func() { saveArmadaLocal = origSave }()

	sp := newSettingsPage()
	m := armadaTestModel(sp)

	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaAdd)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	typeRunes(sp, m, "https://gw.example.com/abc")
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	typeRunes(sp, m, "bad-token")
	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if next := m.handleArmadaMsg(cmd()); next != nil {
		t.Fatal("failed test must not chain into a save")
	}
	if saveCalled {
		t.Fatal("failed test must not persist the remote")
	}
	if len(m.armadaRemotes) != 0 {
		t.Fatalf("registry should stay empty, got %+v", m.armadaRemotes)
	}
	if !strings.Contains(m.message, "Connection test failed") {
		t.Fatalf("message = %q, want connection-test failure", m.message)
	}
}

// TestArmadaDeleteTwoPress verifies the [ delete ] button needs the cache-clear
// two-press confirm: right focuses the button, the first enter arms it, the
// second enter persists the removal.
func TestArmadaDeleteTwoPress(t *testing.T) {
	var saved []configutil.ArmadaRemote
	origSave := saveArmadaLocal
	saveArmadaLocal = func(remotes []configutil.ArmadaRemote) error {
		saved = remotes
		return nil
	}
	defer func() { saveArmadaLocal = origSave }()

	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "https://gw.example.com/abc", Token: "t1"},
		{URL: "https://gw2.example.com/def", Token: "t2"},
	}

	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	if !sp.armadaDeleteFocused {
		t.Fatal("right should focus the delete button")
	}

	if cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("first enter must only arm the confirm")
	}
	if !sp.armadaDeleteConfirm {
		t.Fatal("first enter should arm the confirm")
	}

	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("second enter should return the delete command")
	}
	m.handleArmadaMsg(cmd().(armadaSaveResultMsg))

	if len(saved) != 1 || saved[0].URL != "https://gw2.example.com/def" {
		t.Fatalf("persisted registry mismatch: %+v", saved)
	}
	if len(m.armadaRemotes) != 1 || m.armadaRemotes[0].URL != "https://gw2.example.com/def" {
		t.Fatalf("model registry mismatch: %+v", m.armadaRemotes)
	}
	if sp.armadaBusy || sp.armadaDeleteConfirm || sp.armadaDeleteFocused {
		t.Fatal("delete sub-state should be reset after completion")
	}
}

// TestArmadaDeleteConfirmResetsOnCursorMove verifies moving off the row disarms
// the confirm (the cache-clear pattern's safety property).
func TestArmadaDeleteConfirmResetsOnCursorMove(t *testing.T) {
	sp := newSettingsPage()
	m := armadaTestModel(sp)
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "https://gw.example.com/abc", Token: "t1"}}

	sp.cursor = settingsPositionOf(sp, m, settingsItemArmadaBase)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}) // arm
	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})  // move away

	if sp.armadaDeleteFocused || sp.armadaDeleteConfirm {
		t.Fatal("cursor move must reset the delete sub-cursor and confirm")
	}
}

// TestArmadaStatusValueRendersStates exercises the four indicator states.
func TestArmadaStatusValueRendersStates(t *testing.T) {
	m := armadaTestModel(nil)
	url := "https://gw.example.com/abc"

	if got := armadaStatusValue(m, url); !strings.Contains(got, "status unknown") {
		t.Fatalf("unknown state renders %q", got)
	}
	m.armadaStatus[url] = armadaStatus{state: armadaStatusPinging}
	if got := armadaStatusValue(m, url); !strings.Contains(got, "pinging") {
		t.Fatalf("pinging state renders %q", got)
	}
	m.armadaStatus[url] = armadaStatus{state: armadaStatusConnected}
	if got := armadaStatusValue(m, url); !strings.Contains(got, "connected") {
		t.Fatalf("connected state renders %q", got)
	}
	m.armadaStatus[url] = armadaStatus{state: armadaStatusError, err: "invalid token"}
	got := armadaStatusValue(m, url)
	if !strings.Contains(got, "error") || !strings.Contains(got, "invalid token") {
		t.Fatalf("error state renders %q", got)
	}
}

// TestArmadaEntriesIncludesLocalRegisteredAndBoot verifies the dropdown is
// local + registered remotes + the unregistered boot remote, with the current
// connection flagged from the live env.
func TestArmadaEntriesIncludesLocalRegisteredAndBoot(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "https://boot.example.com/xyz")
	m := armadaTestModel(nil)
	m.bootGateway = "https://boot.example.com/xyz"
	m.bootToken = "boot-token"
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "https://gw.example.com/abc", Token: "t1"}}

	entries := m.armadaEntries()
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (local + registered + boot): %+v", len(entries), entries)
	}
	if entries[0].displayName != "local" || entries[0].url != "" || entries[0].current {
		t.Fatalf("entry 0 should be non-current local: %+v", entries[0])
	}
	if entries[1].url != "https://gw.example.com/abc" || entries[1].current || entries[1].displayName != "gw.example.com" {
		t.Fatalf("entry 1 should be the non-current registered remote shown by hostname: %+v", entries[1])
	}
	if entries[2].url != "https://boot.example.com/xyz" || !entries[2].current || entries[2].token != "boot-token" {
		t.Fatalf("entry 2 should be the current boot remote: %+v", entries[2])
	}
	if entries[2].displayName != "boot.example.com (env)" {
		t.Fatalf("boot entry should display hostname + (env): %q", entries[2].displayName)
	}

	// A registered remote that matches the boot URL must not be duplicated.
	m.armadaRemotes = append(m.armadaRemotes, configutil.ArmadaRemote{URL: "https://boot.example.com/xyz", Token: "t2"})
	if entries := m.armadaEntries(); len(entries) != 3 {
		t.Fatalf("boot remote duplicated: %+v", entries)
	}
}

// TestSwitchArmadaSwapsEnvAndClearsCaches verifies the live switch rewrites
// the env vars (the single switch point every dial path re-reads) and blanks
// every daemon-derived cache.
func TestSwitchArmadaSwapsEnvAndClearsCaches(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "https://old.example.com/old")
	t.Setenv("FLEET_TOKEN", "old-token")
	m := armadaTestModel(nil)
	m.runtime["alpha/inst"] = nil
	m.creating["alpha/inst"] = true

	cmd := m.switchArmada(armadaEntry{displayName: "local"})
	if cmd == nil {
		t.Fatal("switch should return the reload command")
	}
	if os.Getenv("FLEET_GATEWAY") != "" || os.Getenv("FLEET_TOKEN") != "" {
		t.Fatal("switching to local must clear FLEET_GATEWAY/FLEET_TOKEN")
	}
	if len(m.runtime) != 0 || len(m.creating) != 0 {
		t.Fatal("daemon-derived caches must be cleared on switch")
	}

	cmd = m.switchArmada(armadaEntry{displayName: "new.example.com", url: "https://new.example.com/new", token: "new-token"})
	if cmd == nil {
		t.Fatal("switch should return the reload command")
	}
	if os.Getenv("FLEET_GATEWAY") != "https://new.example.com/new" || os.Getenv("FLEET_TOKEN") != "new-token" {
		t.Fatalf("switching to a remote must set the env vars, got %q/%q", os.Getenv("FLEET_GATEWAY"), os.Getenv("FLEET_TOKEN"))
	}
}

// TestUpdateArmadaSelectSwitchesOnEnter verifies the dropdown: enter on a
// non-current entry triggers the switch; enter on the current entry is a no-op.
func TestUpdateArmadaSelectSwitchesOnEnter(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	os.Unsetenv("FLEET_GATEWAY")
	os.Unsetenv("FLEET_TOKEN")

	m := armadaTestModel(nil)
	fp := m.fleetPage
	m.armadaRemotes = []configutil.ArmadaRemote{{URL: "https://gw.example.com/abc", Token: "t1"}}

	// Enter on "local" (current): no switch, dropdown closes.
	fp.mode = viewArmadaSelect
	fp.armadaDialogRow = 0
	if cmd := fp.updateArmadaSelect(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("selecting the current entry must not switch")
	}
	if fp.mode != viewNormal {
		t.Fatal("dropdown should close on enter")
	}

	// Enter on the registered remote: switches (env swapped).
	fp.mode = viewArmadaSelect
	fp.armadaDialogRow = 1
	if cmd := fp.updateArmadaSelect(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("selecting another entry should return the switch command")
	}
	if os.Getenv("FLEET_GATEWAY") != "https://gw.example.com/abc" {
		t.Fatalf("FLEET_GATEWAY = %q after switch", os.Getenv("FLEET_GATEWAY"))
	}
}

// TestViewFleetListEmbedsArmadaSelector verifies the main page's top border
// carries the selector with the current connection name.
func TestViewFleetListEmbedsArmadaSelector(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_TOKEN", "")
	os.Unsetenv("FLEET_GATEWAY")
	os.Unsetenv("FLEET_TOKEN")

	m := armadaTestModel(nil)
	// "Armada" wears a per-character gradient, so strip ANSI before matching the
	// visible border text (what the terminal actually shows).
	out := ansi.Strip(m.fleetPage.viewFleetList(m))
	if !strings.Contains(out, "Armada [ local ]") {
		t.Fatal("fleet list view missing the Armada border selector")
	}
	if m.fleetPage.armadaY < 0 || m.fleetPage.armadaX1 <= m.fleetPage.armadaX0 {
		t.Fatalf("armada hit-test span not recorded: y=%d x0=%d x1=%d",
			m.fleetPage.armadaY, m.fleetPage.armadaX0, m.fleetPage.armadaX1)
	}
}

// TestArmadaSelectorMouseClickOpensDropdown drives a real left-click through
// model.Update at the recorded selector span and asserts it focuses the
// selector and opens the dropdown (the ticket requires mouse reachability).
func TestArmadaSelectorMouseClickOpensDropdown(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	os.Unsetenv("FLEET_GATEWAY")
	os.Unsetenv("FLEET_TOKEN")

	m := *armadaTestModel(nil)
	m.currentPage = m.fleetPage
	// Render once so armadaY/X0/X1 are recorded for hit-testing.
	m.fleetPage.viewFleetList(&m)
	fp := m.fleetPage

	click := tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      fp.armadaX0,
		Y:      fp.armadaY,
	}
	next, _ := m.Update(click)
	if next.(model).fleetPage.mode != viewArmadaSelect {
		t.Fatalf("click on the selector should open the dropdown, mode = %v", next.(model).fleetPage.mode)
	}

	// A click well outside the label span must NOT open the selector.
	fp.mode = viewNormal
	miss := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: fp.armadaX1 + 5, Y: fp.armadaY}
	next, _ = m.Update(miss)
	if next.(model).fleetPage.mode == viewArmadaSelect {
		t.Fatal("a click outside the selector span must not open the dropdown")
	}
}

// TestArmadaEntriesIncludesFleetServerBoot verifies a FLEET_SERVER-booted TUI
// is reflected: 'local' is NOT marked current, the server boot endpoint is a
// selectable '(env)' entry that IS current, and its key round-trips.
func TestArmadaEntriesIncludesFleetServerBoot(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "10.0.0.5:50051")
	os.Unsetenv("FLEET_GATEWAY")

	m := armadaTestModel(nil)
	m.bootServer = "10.0.0.5:50051"

	entries := m.armadaEntries()
	if entries[0].displayName != "local" || entries[0].current {
		t.Fatalf("with FLEET_SERVER set, 'local' must not be current: %+v", entries[0])
	}
	last := entries[len(entries)-1]
	if last.server != "10.0.0.5:50051" || !last.current {
		t.Fatalf("FLEET_SERVER boot endpoint should be the current entry: %+v", last)
	}
	if last.displayName != "10.0.0.5 (env)" {
		t.Fatalf("FLEET_SERVER entry should display host + (env): %q", last.displayName)
	}

	// Switching to local from a FLEET_SERVER boot must clear FLEET_SERVER.
	m.switchArmada(armadaEntry{displayName: "local"})
	if os.Getenv("FLEET_SERVER") != "" {
		t.Fatal("switching to local must unset FLEET_SERVER")
	}
}

// TestArmadaDisplayNameHostnameAndCollision verifies entries are shown by
// hostname, and that two gateways on the SAME host are disambiguated by the
// first 8 characters of their session id (and the border matches).
func TestArmadaDisplayNameHostnameAndCollision(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "https://fleet.cluster.bbenetti.ca/grpc/8e7d1f0aa9")
	os.Unsetenv("FLEET_SERVER")
	m := armadaTestModel(nil)
	m.bootGateway = "https://fleet.cluster.bbenetti.ca/grpc/8e7d1f0aa9"
	m.armadaRemotes = []configutil.ArmadaRemote{
		{URL: "https://other.example.com/abc123", Token: "t1"},
		// Same host as the boot gateway → both must carry the sid suffix.
		{URL: "https://fleet.cluster.bbenetti.ca/grpc/c4ba2200ff", Token: "t2"},
	}

	byHost := map[string]string{} // url -> displayName
	for _, e := range m.armadaEntries() {
		byHost[e.url] = e.displayName
	}
	if got := byHost["https://other.example.com/abc123"]; got != "other.example.com" {
		t.Fatalf("unique host should show bare hostname, got %q", got)
	}
	if got := byHost["https://fleet.cluster.bbenetti.ca/grpc/c4ba2200ff"]; got != "fleet.cluster.bbenetti.ca - c4ba2200" {
		t.Fatalf("colliding host should show host - sid8, got %q", got)
	}
	// The boot gateway shares the host AND is the env entry → host - sid8 (env).
	if got := byHost["https://fleet.cluster.bbenetti.ca/grpc/8e7d1f0aa9"]; got != "fleet.cluster.bbenetti.ca - 8e7d1f0a (env)" {
		t.Fatalf("colliding env host should show host - sid8 (env), got %q", got)
	}

	// The top-border display matches the current connection's dropdown name.
	if got := m.armadaCurrentDisplay(); got != "fleet.cluster.bbenetti.ca - 8e7d1f0a (env)" {
		t.Fatalf("border display should match the current entry, got %q", got)
	}
}

// TestArmadaNavCycle verifies the selector is part of the j/k cycle: up from the
// top row focuses it, up again wraps to the bottom row, and down from the
// selector returns to the top row.
func TestArmadaNavCycle(t *testing.T) {
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha"},
		{kind: rowSettings},
	}
	fp.cursor = 0
	m := &model{fleetPage: fp, armadaStatus: make(map[string]armadaStatus)}

	// Up from the top row focuses the selector.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyUp})
	if !fp.armadaFocused {
		t.Fatal("up from the top row should focus the Armada selector")
	}
	// Up again wraps to the bottom row.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyUp})
	if fp.armadaFocused || fp.cursor != 2 {
		t.Fatalf("up from the selector should wrap to the bottom row, focused=%v cursor=%d", fp.armadaFocused, fp.cursor)
	}
	// Down from the bottom row focuses the selector again.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyDown})
	if !fp.armadaFocused {
		t.Fatal("down from the bottom row should focus the Armada selector")
	}
	// Down from the selector returns to the top row.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.armadaFocused || fp.cursor != 0 {
		t.Fatalf("down from the selector should land on the top row, focused=%v cursor=%d", fp.armadaFocused, fp.cursor)
	}
	// Enter while focused opens the dropdown (mode flips; the ping cmd may be
	// nil when no remotes are registered).
	fp.armadaFocused = true
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyEnter})
	if fp.mode != viewArmadaSelect {
		t.Fatalf("enter on the focused selector should open the dropdown, mode=%v", fp.mode)
	}
}

// TestWatchGenerationDropsStaleEvents verifies the generation guard: a
// stateChangedMsg from a superseded connection is ignored, while one matching
// the active generation is applied.
func TestWatchGenerationDropsStaleEvents(t *testing.T) {
	m := *armadaTestModel(nil)
	m.currentPage = m.fleetPage
	m.watchGen = 7

	state := &fleetgrpc.State{Fleets: map[string]*fleetgrpc.Fleet{
		"alpha": {Name: "alpha"},
	}}

	// Stale gen: dropped, m.st stays empty.
	next, _ := m.Update(stateChangedMsg{state: state, gen: 6})
	if len(next.(model).st.Fleets) != 0 {
		t.Fatal("a stale-generation state event must be dropped")
	}

	// Current gen: applied.
	next, _ = m.Update(stateChangedMsg{state: state, gen: 7})
	if _, ok := next.(model).st.Fleets["alpha"]; !ok {
		t.Fatal("a current-generation state event must be applied")
	}
}

// TestBounceWatchStreamBumpsGeneration verifies bounceWatchStream increments
// the generation (so switchArmada adopts a fresh gen) and that the no-op path
// in tests returns the current gen without panicking.
func TestBounceWatchStreamBumpsGeneration(t *testing.T) {
	// In a unit test the stream was never started, so cancel==nil and bounce
	// is a no-op returning the current gen (0).
	if got := bounceWatchStream(); got != watchCtl.gen {
		t.Fatalf("bounce no-op returned %d, want current gen %d", got, watchCtl.gen)
	}
}

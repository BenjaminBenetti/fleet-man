package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// newMicTestModel returns a settings-page model whose saves land in a temp
// HOME through the setConfigRemote seam.
func newMicTestModel(t *testing.T) (*settingsPage, *model) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	origSetConfig := setConfigRemote
	setConfigRemote = func(c *state.Config) error { return state.SaveConfig(c) }
	t.Cleanup(func() { setConfigRemote = origSetConfig })

	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
	}
	return sp, m
}

func visibleSettingsItems(sp *settingsPage, m *model) []int {
	var items []int
	for i := 0; i < sp.settingsItemCount(m); i++ {
		sp.cursor = i
		items = append(items, sp.settingsCursorItem(m))
	}
	return items
}

// The microphone is opt-in, and while off it is one row — not a form.
func TestMicSectionOffShowsOnlyTheToggle(t *testing.T) {
	sp, m := newMicTestModel(t)
	if m.config.MicSettings.Enabled {
		t.Fatal("the microphone must default to off")
	}
	items := visibleSettingsItems(sp, m)
	if !slices.Contains(items, settingsItemMicEnabled) {
		t.Fatal("Microphone toggle row missing")
	}
	if slices.Contains(items, settingsItemMicDevice) {
		t.Fatal("Device row must be hidden while the microphone is off")
	}
	view := sp.View(m)
	if !strings.Contains(view, "Microphone") {
		t.Fatalf("Microphone section not rendered:\n%s", view)
	}
	if strings.Contains(view, "Status") && strings.Contains(view, "microphone closed") {
		t.Fatal("status read-out must be hidden while off")
	}
}

func TestToggleMicEnabledPersistsAndRevealsDevice(t *testing.T) {
	sp, m := newMicTestModel(t)
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicEnabled)

	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if !m.config.MicSettings.Enabled {
		t.Fatal("enter on the toggle should enable the microphone")
	}
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.MicSettings.Enabled {
		t.Fatal("toggle did not persist")
	}
	if cmd == nil || !m.micDevicesLoading {
		t.Fatal("turning the microphone on should start listing this machine's devices")
	}
	if !slices.Contains(visibleSettingsItems(sp, m), settingsItemMicDevice) {
		t.Fatal("Device row should appear once enabled")
	}

	// And off again.
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicEnabled)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.config.MicSettings.Enabled {
		t.Fatal("second enter should disable")
	}
}

func TestToggleMicEnabledRevertsOnSaveFailure(t *testing.T) {
	sp, m := newMicTestModel(t)
	setConfigRemote = func(*state.Config) error { return errors.New("daemon unreachable") }
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicEnabled)

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.config.MicSettings.Enabled {
		t.Fatal("a failed save must leave the toggle where it was")
	}
	if !strings.Contains(m.message, "Failed to save settings") {
		t.Fatalf("message = %q", m.message)
	}
}

func TestCycleMicDeviceWalksDefaultThenDevices(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings.Enabled = true
	m.micDevicesLoaded = true
	m.micDevices = []mic.Device{{ID: "pulse:yeti", Label: "Yeti Orb"}, {ID: "pulse:webcam", Label: "Webcam"}}
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicDevice)

	right := tea.KeyMsg{Type: tea.KeyRight}
	left := tea.KeyMsg{Type: tea.KeyLeft}

	for _, want := range []string{"pulse:yeti", "pulse:webcam", ""} {
		sp.Update(m, right)
		if got := m.config.MicSettings.Device; got != want {
			t.Fatalf("after right: device = %q, want %q", got, want)
		}
	}
	sp.Update(m, left) // wraps backwards from default to the last device
	if got := m.config.MicSettings.Device; got != "pulse:webcam" {
		t.Fatalf("after left: device = %q, want pulse:webcam", got)
	}
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.MicSettings.Device != "pulse:webcam" {
		t.Fatalf("device not persisted: %q", loaded.MicSettings.Device)
	}
	if !strings.Contains(sp.View(m), "Webcam") {
		t.Fatal("Device row should show the device's label")
	}
}

// The first press on an unlisted selector loads the list instead of guessing.
func TestCycleMicDeviceLoadsTheListFirst(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings.Enabled = true
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicDevice)

	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})

	if cmd == nil || !m.micDevicesLoading {
		t.Fatal("cycling with no list should kick off enumeration")
	}
	if m.config.MicSettings.Device != "" {
		t.Fatalf("device changed to %q before the list loaded", m.config.MicSettings.Device)
	}
}

// A device id saved by a client on another machine is shown for what it will
// do here: record the system default.
func TestMicDeviceLabelForAnUnknownDevice(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings = state.MicSettings{Enabled: true, Device: "avfoundation:MacBook Pro Microphone"}
	m.micDevicesLoaded = true
	label := sp.micDeviceLabel(m)
	if !strings.HasPrefix(label, mic.DefaultLabel) || !strings.Contains(label, "not found here") {
		t.Fatalf("label = %q", label)
	}
}

func TestMicDevicesMsgRecordsAMissingCaptureTool(t *testing.T) {
	_, m := newMicTestModel(t)
	m.micDevicesLoading = true
	next, _ := m.Update(micDevicesMsg{err: mic.ErrNoCaptureTool})
	got := next.(model)
	if got.micDevicesLoading {
		t.Fatal("still marked as loading")
	}
	if got.micDevicesLoaded {
		t.Fatal("a failed listing must stay retryable (the user may install a recorder)")
	}
	if !strings.Contains(got.micDevicesErr, "no capture tool") {
		t.Fatalf("micDevicesErr = %q", got.micDevicesErr)
	}
}

// The header badge is the at-a-glance "is fleet listening?" — live only.
func TestMicLiveIndicatorOnlyWhileLive(t *testing.T) {
	_, m := newMicTestModel(t)
	for _, state := range []mic.State{mic.StateConnecting, mic.StateIdle, mic.StateError, mic.StateDisabled} {
		m.micStatus = mic.Status{State: state}
		if got := micLiveIndicator(m); got != "" {
			t.Fatalf("state %v rendered a badge %q", state, got)
		}
	}
	next, _ := m.Update(micStatusMsg{gen: micGen(), status: mic.Status{State: mic.StateLive, Instances: []string{"alpha/i1"}}})
	live := next.(model)
	if !strings.Contains(micLiveIndicator(&live), "MIC") {
		t.Fatal("no badge while live")
	}
	if !strings.Contains(micStatusValue(&live), "alpha/i1") {
		t.Fatalf("status row should name who is recording: %q", micStatusValue(&live))
	}
}

// Until Run() arms it (so: in every test) the controller must be inert.
func TestSyncMicProviderIsInertBeforeStart(t *testing.T) {
	syncMicProvider(state.MicSettings{Enabled: true, Device: "pulse:yeti"})
	micCtl.mu.Lock()
	running := micCtl.cancel != nil
	micCtl.mu.Unlock()
	if running {
		t.Fatal("a provider goroutine started without startMicControl")
	}
	syncMicFromConfig(nil) // a nil config reads as "off"; must not panic or start anything
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	if micCtl.cancel != nil {
		t.Fatal("a nil config started a provider")
	}
}

// armMicCtl arms the controller with a parent context the test owns, and a
// provider seam that just parks — so start/stop bookkeeping can be observed
// without dialing a daemon. It restores the inert state afterwards.
func armMicCtl(t *testing.T) (started chan int) {
	t.Helper()
	started = make(chan int, 8)
	parent, cancel := context.WithCancel(context.Background())
	origRun := runMicProviderFn
	runMicProviderFn = func(ctx context.Context, _ *tea.Program, gen int) {
		started <- gen
		<-ctx.Done()
	}
	micCtl.mu.Lock()
	micCtl.parent, micCtl.cancel = parent, nil
	micCtl.mu.Unlock()
	t.Cleanup(func() {
		cancel()
		runMicProviderFn = origRun
		micCtl.mu.Lock()
		micCtl.parent, micCtl.program, micCtl.cancel = nil, nil, nil
		micCtl.mu.Unlock()
	})
	return started
}

func micCtlRunning() bool {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	return micCtl.cancel != nil
}

func TestSyncMicProviderStartsAndStops(t *testing.T) {
	started := armMicCtl(t)
	on := state.MicSettings{Enabled: true}

	syncMicProvider(on)
	first := <-started
	if !micCtlRunning() {
		t.Fatal("enable should start a provider")
	}
	syncMicProvider(on) // idempotent: no second provider
	select {
	case gen := <-started:
		t.Fatalf("a second enable started another provider (gen %d)", gen)
	case <-time.After(50 * time.Millisecond):
	}

	syncMicProvider(state.MicSettings{})
	if micCtlRunning() {
		t.Fatal("disable should stop the provider")
	}
	syncMicProvider(on)
	if second := <-started; second <= first {
		t.Fatalf("a restart must get a new generation: %d then %d", first, second)
	}
}

// A provider that ends ON ITS OWN (no recorder on this machine, a daemon that
// predates the RPC) must free the slot, or no later sync could ever restart it
// — the user installs a recorder, and the microphone stays dead until they
// restart the TUI.
func TestMicProviderThatExitsOnItsOwnCanBeRestarted(t *testing.T) {
	started := armMicCtl(t)
	runMicProviderFn = func(_ context.Context, _ *tea.Program, gen int) {
		started <- gen
		micProviderExited(gen) // what runMicProvider's defer does
	}
	on := state.MicSettings{Enabled: true}

	syncMicProvider(on)
	<-started
	waitUntil(t, "the exited provider to free the slot", func() bool { return !micCtlRunning() })

	syncMicProvider(on)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider was not restarted after exiting on its own")
	}
}

// A stale provider's exit must not free a slot its successor now holds.
func TestMicProviderExitIgnoresASupersededGeneration(t *testing.T) {
	started := armMicCtl(t)
	syncMicProvider(state.MicSettings{Enabled: true})
	current := <-started
	micProviderExited(current - 1)
	if !micCtlRunning() {
		t.Fatal("an old generation's exit cleared the current provider's slot")
	}
}

// For a privacy indicator, failing toward "not listening" is the wrong
// direction: a dying provider's parting status must not clear the badge of the
// provider that replaced it.
func TestStaleMicStatusCannotClearTheLiveBadge(t *testing.T) {
	_, m := newMicTestModel(t)
	started := armMicCtl(t)
	syncMicProvider(state.MicSettings{Enabled: true})
	old := <-started
	syncMicProvider(state.MicSettings{})
	syncMicProvider(state.MicSettings{Enabled: true})
	current := <-started

	next, _ := m.Update(micStatusMsg{gen: current, status: mic.Status{State: mic.StateLive, Instances: []string{"alpha/i1"}}})
	live := next.(model)
	next, _ = live.Update(micStatusMsg{gen: old, status: mic.Status{State: mic.StateConnecting}})
	after := next.(model)
	if after.micStatus.State != mic.StateLive {
		t.Fatalf("a superseded provider's status was applied: %+v", after.micStatus)
	}
	if !strings.Contains(micLiveIndicator(&after), "MIC") {
		t.Fatal("the live badge disappeared while the microphone is open")
	}
}

// A failed enumeration (sound server momentarily wedged) must not turn the
// user's real device into "not found here".
func TestMicDevicesErrorKeepsThePreviousList(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings = state.MicSettings{Enabled: true, Device: "pulse:yeti"}
	m.micDevicesLoaded = true
	m.micDevices = []mic.Device{{ID: "pulse:yeti", Label: "Yeti Orb"}}

	next, _ := m.Update(micDevicesMsg{err: errors.New("pactl list sources: timeout")})
	got := next.(model)
	if len(got.micDevices) != 1 {
		t.Fatalf("the device list was wiped: %+v", got.micDevices)
	}
	if label := sp.micDeviceLabel(&got); label != "Yeti Orb" {
		t.Fatalf("label = %q, want the real device", label)
	}
	if !strings.Contains(got.micDevicesErr, "timeout") {
		t.Fatalf("the error should still be shown: %q", got.micDevicesErr)
	}
}

func TestCycleMicDeviceExplainsAnEmptyList(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings.Enabled = true
	m.micDevicesLoaded = true
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicDevice)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	if !strings.Contains(m.message, "system default") {
		t.Fatalf("a no-op key press should say why: %q", m.message)
	}
}

func TestMicStatusRowFlagsADeviceFallback(t *testing.T) {
	_, m := newMicTestModel(t)
	m.micStatus = mic.Status{State: mic.StateLive, Instances: []string{"alpha/i1"}, FellBack: true}
	if !strings.Contains(micStatusValue(m), "recording the system default") {
		t.Fatalf("status = %q", micStatusValue(m))
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The FIRST enumeration failing (no earlier list to fall back on) must not mark
// the list as loaded: that would claim "not found here" about a device nobody
// looked for, answer ←/→ with "no devices", and block the retry.
func TestFirstMicDevicesErrorStaysRetryable(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings = state.MicSettings{Enabled: true, Device: "pulse:yeti"}
	m.micDevicesLoading = true

	next, _ := m.Update(micDevicesMsg{err: errors.New("pactl list sources: timeout")})
	got := next.(model)
	if got.micDevicesLoaded {
		t.Fatal("a failed listing must not count as loaded")
	}
	if label := sp.micDeviceLabel(&got); strings.Contains(label, "not found") {
		t.Fatalf("label = %q: we never established that", label)
	}
	sp.cursor = settingsPositionOf(sp, &got, settingsItemMicDevice)
	if cmd := sp.Update(&got, tea.KeyMsg{Type: tea.KeyRight}); cmd == nil || !got.micDevicesLoading {
		t.Fatal("the next key press should retry the listing")
	}
}

// mic.Run's report callback must not block: it is called from the loop that
// closes the microphone when demand ends, and bubbletea's Send parks until the
// current Update returns (which can be seconds, mid-reload). The forwarder takes
// the status and returns at once; only the latest matters; nothing is lost at
// the end.
func TestMicStatusForwarderNeverBlocksTheProvider(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var delivered []mic.State
	report, flush := newMicStatusForwarder(func(status mic.Status) {
		<-release // a slow Update
		mu.Lock()
		delivered = append(delivered, status.State)
		mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		for _, state := range []mic.State{mic.StateLive, mic.StateError, mic.StateLive, mic.StateIdle} {
			report(mic.Status{State: state})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("report blocked behind a slow Send — the microphone would be held open")
	}

	close(release)
	flush()
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) == 0 || delivered[len(delivered)-1] != mic.StateIdle {
		t.Fatalf("delivered %v: the final status must arrive", delivered)
	}
	report(mic.Status{State: mic.StateLive}) // after flush: a no-op, not a panic
}

// Turning the microphone off must not clear the badge before the recorder is
// really gone; the stopping provider's parting status does that.
func TestDisablingDoesNotClearTheLiveBadgeEarly(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings.Enabled = true
	m.micStatus = mic.Status{State: mic.StateLive, Instances: []string{"alpha/i1"}}
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicEnabled)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.config.MicSettings.Enabled {
		t.Fatal("setup: the toggle should now be off")
	}
	if micLiveIndicator(m) == "" {
		t.Fatal("the badge was cleared synchronously, while the recorder is still being torn down")
	}
}

func TestDeviceChangeWhileLiveSaysItAppliesNextTime(t *testing.T) {
	sp, m := newMicTestModel(t)
	m.config.MicSettings.Enabled = true
	m.micDevicesLoaded = true
	m.micDevices = []mic.Device{{ID: "pulse:yeti", Label: "Yeti Orb"}}
	m.micStatus = mic.Status{State: mic.StateLive}
	sp.cursor = settingsPositionOf(sp, m, settingsItemMicDevice)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyRight})
	if !strings.Contains(m.message, "next recording") {
		t.Fatalf("message = %q", m.message)
	}
}

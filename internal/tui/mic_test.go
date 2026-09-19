package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

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
	if got.micDevicesLoading || !got.micDevicesLoaded {
		t.Fatal("loading flags not settled")
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
	next, _ := m.Update(micStatusMsg{status: mic.Status{State: mic.StateLive, Instances: []string{"alpha/i1"}}})
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
	if micDevice() != "pulse:yeti" {
		t.Fatal("the device should still be recorded")
	}
	syncMicFromConfig(nil)
	if micDevice() != "" {
		t.Fatal("a nil config reads as defaults")
	}
}

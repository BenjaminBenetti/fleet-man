package tui

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

func newOutputTestModel(t *testing.T) (*settingsPage, *model) {
	t.Helper()
	p, m := newMicTestModel(t)
	original := setOutputConfigRemote
	var revision uint64
	setOutputConfigRemote = func(c *configutil.Config) (uint64, error) {
		revision++
		return revision, state.SaveConfig(c)
	}
	t.Cleanup(func() { setOutputConfigRemote = original })
	return p, m
}

func TestOutputSelectorPersistsRemoteDeviceAndAuto(t *testing.T) {
	sp, m := newOutputTestModel(t)
	if !m.config.OutputSettings.Enabled || !slices.Contains(visibleSettingsItems(sp, m), settingsItemOutputDevice) || outputDeviceLabel(m) != "Auto" {
		t.Fatal("output must default to on with Auto visible")
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemOutputEnabled)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.config.OutputSettings.Enabled || slices.Contains(visibleSettingsItems(sp, m), settingsItemOutputDevice) {
		t.Fatal("disabling output did not hide the selector")
	}
	saved, err := state.LoadConfig()
	if err != nil || saved.OutputSettings.Enabled {
		t.Fatalf("disabled output was not persisted: %v %v", saved, err)
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemOutputEnabled)
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.config.OutputSettings.Enabled || m.config.MicSettings.Enabled {
		t.Fatal("output toggle changed microphone")
	}
	m.outputTargets = &fleetgrpc.OutputTargets{Targets: []*fleetgrpc.OutputTarget{{Client: "remote-desktop", Devices: []*fleetgrpc.OutputDevice{{Id: "pulse:usb", Label: "Speakers"}}, DevicesListed: true}}}
	sp.cycleOutputDevice(m, 1)
	sp.cycleOutputDevice(m, 1)
	cfg, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutputSettings.Client != "remote-desktop" || cfg.OutputSettings.Device != "pulse:usb" {
		t.Fatalf("selection: %+v", cfg.OutputSettings)
	}
	sp.cycleOutputDevice(m, 1)
	if m.config.OutputSettings.Client != "" || outputDeviceLabel(m) != "Auto" {
		t.Fatal("selector did not cycle back to Auto")
	}
	old := m.config.OutputSettings
	setOutputConfigRemote = func(*configutil.Config) (uint64, error) { return 0, errors.New("offline") }
	sp.cycleOutputDevice(m, 1)
	if m.config.OutputSettings != old {
		t.Fatal("failed save changed selection")
	}
}

func TestOutputWatchCannotUndoNewerLocalSelection(t *testing.T) {
	p, m := newOutputTestModel(t)
	m.watchGen = 1
	m.outputTargets = &fleetgrpc.OutputTargets{Targets: []*fleetgrpc.OutputTarget{{Client: "desk"}}}
	p.cycleOutputDevice(m, 1)
	newRevision := m.outputRevision
	stale := &fleetgrpc.OutputTargets{Revision: newRevision - 1, Settings: &fleetgrpc.OutputSettings{Enabled: true}}
	next, _ := m.Update(outputTargetsMsg{targets: stale, gen: 1})
	m2 := next.(model)
	if m2.config.OutputSettings.Client != "desk" || m2.outputRevision != newRevision {
		t.Fatal("queued Watch update undid the saved selection")
	}
	stale.Revision = newRevision + 1
	next, _ = m2.Update(outputTargetsMsg{targets: stale, gen: 1})
	m2 = next.(model)
	if m2.config.OutputSettings.Client != "" {
		t.Fatal("newer selection from another client was ignored")
	}
}
func TestOutputWatchRejectsStaleDaemonAndAdoptsSettings(t *testing.T) {
	_, m := newMicTestModel(t)
	m.watchGen = 2
	pushed := &fleetgrpc.OutputTargets{Settings: &fleetgrpc.OutputSettings{Enabled: false, Client: "desk", Device: "pulse:usb"}}
	next, _ := m.Update(outputTargetsMsg{targets: pushed, gen: 1})
	m2 := next.(model)
	if !m2.config.OutputSettings.Enabled || m2.config.OutputSettings.Client != "" {
		t.Fatal("stale daemon settings adopted")
	}
	next, _ = m2.Update(outputTargetsMsg{targets: pushed, gen: 2})
	m2 = next.(model)
	if m2.config.OutputSettings.Enabled || m2.config.OutputSettings.Client != "desk" {
		t.Fatal("current daemon selection ignored")
	}
}

func TestSwitchArmadaStopsOutputUntilConfigArrives(t *testing.T) {
	for _, key := range []string{"FLEET_GATEWAY", "FLEET_TOKEN", "FLEET_SSH", "FLEET_SERVER", "TMUX"} {
		t.Setenv(key, "")
	}
	m := armadaTestModel(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outputCtl.mu.Lock()
	oldParent, oldCancel, oldGen := outputCtl.parent, outputCtl.cancel, outputCtl.gen
	outputCtl.parent, outputCtl.cancel = context.Background(), cancel
	outputCtl.mu.Unlock()
	t.Cleanup(func() {
		outputCtl.mu.Lock()
		defer outputCtl.mu.Unlock()
		outputCtl.parent, outputCtl.cancel, outputCtl.gen = oldParent, oldCancel, oldGen
	})
	m.switchArmada(armadaEntry{url: "https://new.example.com/fleet", token: "test-token"})
	if ctx.Err() == nil {
		t.Fatal("switch left playback connected to the previous daemon")
	}
	if !m.armadaConfigPending || m.config.OutputSettings.Enabled {
		t.Fatal("placeholder defaults enabled output before destination config arrived")
	}
}

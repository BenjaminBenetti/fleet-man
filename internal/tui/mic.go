package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
)

// mic.go is the TUI's side of the virtual microphone. The TUI is the natural
// microphone PROVIDER: it runs on the machine the human sits at, which is where
// the microphone is — also when the daemon it drives is remote. While the
// feature is enabled it holds a Mic stream to the daemon (internal/mic.Run),
// which opens the real microphone only while something inside an instance is
// recording. The settings page's Microphone section drives it; the header shows
// when the microphone is live.

// micCtl owns the provider goroutine. Like watchCtl it is package state rather
// than model state: the goroutine outlives any one Update.
var micCtl struct {
	mu      sync.Mutex
	parent  context.Context
	program *tea.Program
	cancel  context.CancelFunc // non-nil while the provider is running
	device  string             // consulted by the provider at each capture start
}

// startMicControl arms micCtl for the TUI's lifetime. Cancelling parent stops
// the provider. Until this runs (tests) every micCtl call is a no-op.
func startMicControl(parent context.Context, program *tea.Program) {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	micCtl.parent = parent
	micCtl.program = program
}

// syncMicFromConfig is syncMicProvider for a config that may not have arrived
// yet (nil reads as the feature being off).
func syncMicFromConfig(config *configutil.Config) {
	if config == nil {
		syncMicProvider(configutil.MicSettings{})
		return
	}
	syncMicProvider(config.MicSettings)
}

// syncMicProvider converges the provider on the settings: running iff enabled,
// recording from device. Idempotent — called wherever m.config is replaced, so
// an armada switch moves the provider too: the switch blanks the config (stop),
// and the new daemon's config decides whether to start against IT.
func syncMicProvider(settings configutil.MicSettings) {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	micCtl.device = settings.Device
	if micCtl.parent == nil {
		return
	}
	switch {
	case settings.Enabled && micCtl.cancel == nil:
		ctx, cancel := context.WithCancel(micCtl.parent)
		micCtl.cancel = cancel
		go runMicProvider(ctx, micCtl.program)
	case !settings.Enabled && micCtl.cancel != nil:
		micCtl.cancel()
		micCtl.cancel = nil
	}
}

func micDevice() string {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	return micCtl.device
}

// micStatusMsg carries a provider status change into the bubbletea loop.
type micStatusMsg struct{ status mic.Status }

// runMicProvider is the provider goroutine: dial, then hold the Mic stream
// until ctx is cancelled. mic.Run reconnects the stream itself; the loop here
// only covers the dial, which can fail while a daemon is still coming up.
func runMicProvider(ctx context.Context, program *tea.Program) {
	report := func(status mic.Status) { program.Send(micStatusMsg{status: status}) }
	defer func() {
		if ctx.Err() != nil {
			// Stopped on purpose (disabled, or an armada switch): clear the
			// indicator. A successor reports its own state right after.
			report(mic.Status{State: mic.StateConnecting})
		}
	}()

	// A machine that cannot record must not attach at all: attaching makes it
	// the daemon's ACTIVE provider, silencing a working one elsewhere.
	if !mic.Available() {
		report(mic.Status{State: mic.StateError, Detail: "no capture tool: " + mic.InstallHint()})
		return
	}

	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			report(mic.Status{State: mic.StateConnecting})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		mic.Run(ctx, conn.Service(), micDevice, report)
		conn.Close()
		return
	}
}

// --- device list ----------------------------------------------------------------

// micDevicesMsg carries the result of enumerating this machine's capture devices.
type micDevicesMsg struct {
	devices []mic.Device
	err     error
}

// fetchMicDevicesCmd enumerates capture devices off the Update loop (it shells
// out to the sound server, which can take seconds when that is wedged).
func fetchMicDevicesCmd() tea.Cmd {
	return func() tea.Msg {
		devices, err := mic.Devices()
		return micDevicesMsg{devices: devices, err: err}
	}
}

// ensureMicDevices kicks off a device enumeration unless one has run (or is
// running). Returns nil when there is nothing to do.
func (m *model) ensureMicDevices() tea.Cmd {
	if m.micDevicesLoaded || m.micDevicesLoading {
		return nil
	}
	m.micDevicesLoading = true
	return fetchMicDevicesCmd()
}

// --- settings page --------------------------------------------------------------

// toggleMicEnabled flips the virtual microphone on/off and saves. Reverts on a
// save failure, mirroring the other toggles.
func (settingsPage *settingsPage) toggleMicEnabled(m *model) tea.Cmd {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.MicSettings.Enabled
	m.config.MicSettings.Enabled = !current
	if err := setConfigRemote(m.config); err != nil {
		m.config.MicSettings.Enabled = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return nil
	}
	syncMicProvider(m.config.MicSettings)
	if current {
		m.micStatus = mic.Status{}
		m.message = "Microphone off — instances no longer have a virtual microphone"
		return nil
	}
	m.message = "Microphone on — it is only opened while an instance is recording"
	return m.ensureMicDevices()
}

// cycleMicDevice steps the capture device through [system default, devices…]
// and saves. The list is enumerated lazily; the first press just loads it.
func (settingsPage *settingsPage) cycleMicDevice(m *model, direction int) tea.Cmd {
	if m.config == nil {
		return nil
	}
	if !m.micDevicesLoaded {
		return m.ensureMicDevices()
	}
	ids := []string{""}
	for _, device := range m.micDevices {
		ids = append(ids, device.ID)
	}
	current := m.config.MicSettings.Device
	index := 0 // an unknown id (a device from another machine) counts as default
	for i, id := range ids {
		if id == current {
			index = i
			break
		}
	}
	next := ids[(index+direction+len(ids))%len(ids)]
	if next == current {
		return nil
	}
	m.config.MicSettings.Device = next
	if err := setConfigRemote(m.config); err != nil {
		m.config.MicSettings.Device = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return nil
	}
	// Applies to the next recording; a capture already running keeps its device.
	syncMicProvider(m.config.MicSettings)
	m.message = fmt.Sprintf("Microphone set to %s", settingsPage.micDeviceLabel(m))
	return nil
}

// micDeviceLabel names the configured device. An id this machine does not have
// (a config shared with a client elsewhere) is shown for what it will do.
func (settingsPage *settingsPage) micDeviceLabel(m *model) string {
	id := m.config.MicSettings.Device
	if id == "" {
		return mic.DefaultLabel
	}
	for _, device := range m.micDevices {
		if device.ID == id {
			return device.Label
		}
	}
	if m.micDevicesLoaded {
		return mic.DefaultLabel + " (" + id + " not found here)"
	}
	return id
}

// micDeviceValue renders the Device row's value.
func (settingsPage *settingsPage) micDeviceValue(m *model) string {
	if m.micDevicesLoading {
		return m.spinner.View() + " listing devices..."
	}
	value := fmt.Sprintf("[ %s ]", settingsPage.micDeviceLabel(m))
	if m.micDevicesErr != "" {
		value += "\n" + strings.Repeat(" ", 21) + dimStyle.Render(m.micDevicesErr)
	}
	return value
}

// micStatusValue renders the (non-navigable) Status row from the provider's
// latest report.
func micStatusValue(m *model) string {
	switch m.micStatus.State {
	case mic.StateLive:
		return statusRunningStyle.Render("● live") + "  " + dimStyle.Render("→ "+strings.Join(m.micStatus.Instances, ", "))
	case mic.StateIdle:
		return dimStyle.Render("idle — microphone closed until an instance records")
	case mic.StateError:
		return statusCreatingStyle.Render("error") + "  " + dimStyle.Render(m.micStatus.Detail)
	case mic.StateUnsupported:
		return statusCreatingStyle.Render("unsupported") + "  " + dimStyle.Render("this daemon predates the microphone; update it")
	case mic.StateDisabled:
		return dimStyle.Render("disabled on the daemon")
	default:
		return m.spinner.View() + " connecting…"
	}
}

// micLiveIndicator is the header badge shown while the real microphone is open.
// It is the user's at-a-glance answer to "is fleet listening right now?", on
// every page, not just Settings.
func micLiveIndicator(m *model) string {
	if m.micStatus.State != mic.StateLive {
		return ""
	}
	return statusCreatingStyle.Render("● MIC")
}

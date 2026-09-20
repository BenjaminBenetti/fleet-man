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
	cancel  context.CancelFunc // non-nil while a provider goroutine is running
	// gen counts provider STARTS. Every status message carries the gen of the
	// goroutine that sent it and the model drops the ones from a superseded
	// provider (same scheme as watchCtl.gen): a dying provider's parting
	// "connecting" must never clear the live badge of its successor.
	gen int
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

// syncMicProvider converges the provider on the settings: running iff enabled.
// (WHICH device is not the provider's to track: the daemon pushes the selection
// with every demand.) Idempotent — called wherever m.config is replaced, so
// an armada switch moves the provider too: the switch blanks the config (stop),
// and the new daemon's config decides whether to start against IT.
func syncMicProvider(settings configutil.MicSettings) {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	if micCtl.parent == nil {
		return
	}
	switch {
	case settings.Enabled && micCtl.cancel == nil:
		ctx, cancel := context.WithCancel(micCtl.parent)
		micCtl.cancel = cancel
		micCtl.gen++
		go runMicProviderFn(ctx, micCtl.program, micCtl.gen)
	case !settings.Enabled && micCtl.cancel != nil:
		// gen is NOT bumped on stop: the stopping provider's final status is
		// the one that clears the read-out.
		micCtl.cancel()
		micCtl.cancel = nil
	}
}

// micProviderExited is a provider goroutine's last act. If it is still the
// current one, the slot is freed so a later syncMicProvider can start a fresh
// provider: a provider can end ON ITS OWN (no capture tool on this machine, a
// daemon that predates the RPC), and both of those are fixable without
// restarting the TUI — install a recorder, update the daemon.
func micProviderExited(gen int) {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	if micCtl.gen == gen && micCtl.cancel != nil {
		micCtl.cancel() // release the context; nothing is running under it
		micCtl.cancel = nil
	}
}

// micGen is the generation of the current (or most recent) provider.
func micGen() int {
	micCtl.mu.Lock()
	defer micCtl.mu.Unlock()
	return micCtl.gen
}

// runMicProviderFn is the provider goroutine's entry point; a var so tests can
// observe start/stop bookkeeping without dialing a daemon.
var runMicProviderFn = runMicProvider

// micStatusMsg carries a provider status change into the bubbletea loop, stamped
// with the generation of the provider that sent it (see micCtl.gen).
type micStatusMsg struct {
	status mic.Status
	gen    int
}

// runMicProvider is the provider goroutine: dial, then hold the Mic stream
// until ctx is cancelled. mic.Run reconnects the stream itself; the loop here
// only covers the dial, which can fail while a daemon is still coming up.
func runMicProvider(ctx context.Context, program *tea.Program, gen int) {
	report, flush := newMicStatusForwarder(func(status mic.Status) {
		program.Send(micStatusMsg{status: status, gen: gen})
	})
	defer flush() // runs LAST: the parting status below must still be delivered
	defer func() {
		if ctx.Err() != nil {
			// Stopped on purpose (disabled, or an armada switch): clear the
			// read-out. If a successor has already started, this message is
			// stale by gen and the model drops it.
			report(mic.Status{State: mic.StateConnecting})
		}
		micProviderExited(gen)
	}()

	// A machine that cannot record must not attach at all: attaching makes it
	// the daemon's ACTIVE provider, silencing a working one elsewhere.
	if err := mic.Unavailable(); err != nil {
		report(mic.Status{State: mic.StateError, Detail: mic.Describe(err)})
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
		mic.Run(ctx, conn.Service(), "", report)
		conn.Close()
		return
	}
}

// newMicStatusForwarder decouples mic.Run from the bubbletea loop. mic.Run's
// report callback MUST NOT BLOCK — it is called from the very select loop that
// closes the microphone when demand ends — but program.Send is a hand-off onto
// an unbuffered channel whose only reader runs Update inline, and Update does
// blocking RPCs (a reload can take seconds against a remote daemon). Sent
// directly, a slow Update would park the provider loop and hold the real
// microphone open past the end of a recording.
//
// report therefore only deposits the status in a one-slot, latest-wins mailbox;
// a single forwarder goroutine does the (blocking) Send, preserving order. A
// status overwritten before it was sent is one nobody needed: only the current
// state matters to the read-out. flush stops the forwarder after it has
// delivered whatever is pending.
func newMicStatusForwarder(send func(mic.Status)) (report func(mic.Status), flush func()) {
	mailbox := make(chan mic.Status, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for status := range mailbox {
			send(status)
		}
	}()
	var mu sync.Mutex
	closed := false
	report = func(status mic.Status) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		for {
			select {
			case mailbox <- status:
				return
			default:
			}
			select {
			case <-mailbox: // drop the stale one; latest wins
			default:
			}
		}
	}
	flush = func() {
		mu.Lock()
		closed = true
		close(mailbox)
		mu.Unlock()
		<-done
	}
	return report, flush
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
		// m.micStatus is deliberately NOT cleared here. Stopping the provider is
		// asynchronous (the recorder is still being torn down), and a badge that
		// says "not listening" while the microphone is still open is the wrong
		// direction to be wrong in. The stopping provider's own parting status
		// clears the read-out once it really has stopped.
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
	if len(m.micDevices) == 0 {
		// Legitimate (SoX / ffmpeg can only record the default; so can a
		// FLEET_MIC_CAPTURE override) — but a key press that does nothing
		// looks broken, so say why.
		m.message = "No selectable capture devices on this machine — recording the system default"
		return nil
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
	// The daemon pushes the new selection to the provider; it applies to the next
	// recording — a capture already running keeps its device, so say so, or the
	// message claims a switch that has not happened yet.
	m.message = fmt.Sprintf("Microphone set to %s", settingsPage.micDeviceLabel(m))
	if m.micStatus.State == mic.StateLive {
		m.message += " (from the next recording — this one keeps its device)"
	}
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
		value := statusRunningStyle.Render("● live") + "  " + dimStyle.Render("→ "+strings.Join(m.micStatus.Instances, ", "))
		if m.micStatus.FellBack {
			value += "\n" + strings.Repeat(" ", 21) + dimStyle.Render("configured device not found here — recording the system default")
		}
		return value
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

// micLiveIndicator is the badge shown while the real microphone is open: the
// user's at-a-glance answer to "is fleet listening right now?". It sits in the
// fleet page's header and in the Settings page's title (whose Status row spells
// out the same thing, but may be scrolled out of view).
func micLiveIndicator(m *model) string {
	if m.micStatus.State != mic.StateLive {
		return ""
	}
	return statusCreatingStyle.Render("● MIC")
}

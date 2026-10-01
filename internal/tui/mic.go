package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
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
//
// Every TUI on a daemon is such a provider, so there can be several — one per
// machine the human moves between. The daemon records from ONE of them, and the
// settings page chooses which: it lists the capture devices of every attached
// client (this machine's from its own enumeration, the others' as pushed by the
// daemon), and the choice is a client plus a device on it.

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
	return newLatestForwarder(send)
}

// newLatestForwarder is the mailbox behind newMicStatusForwarder, for any
// provider whose status reports must never block the provider loop (the SSH
// agent provider uses it too): report deposits the value in a one-slot,
// latest-wins mailbox and a single forwarder goroutine does the blocking send.
func newLatestForwarder[T any](send func(T)) (report func(T), flush func()) {
	mailbox := make(chan T, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for status := range mailbox {
			send(status)
		}
	}()
	var mu sync.Mutex
	closed := false
	report = func(status T) {
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

// adoptMicSettings takes over the microphone settings the daemon pushed along
// with its client list. This TUI fetched the config once; the settings can be
// changed from any other client since, and the copy here is what the Source row
// shows AND what the next save — of any setting at all, SetConfig sends the
// whole config — writes back. A stale copy would show the wrong microphone and
// then quietly move it. nil means the daemon could not say (or predates the
// push): keep what we have.
func (m *model) adoptMicSettings(pushed *fleetgrpc.MicSettings) {
	if pushed == nil || m.config == nil {
		return
	}
	settings := configutil.MicSettings{Enabled: pushed.GetEnabled(), Device: pushed.GetDevice(), Client: pushed.GetClient()}
	if m.config.MicSettings == settings {
		return
	}
	m.config.MicSettings = settings
	// Enabled may have flipped too (another client turned the microphone off,
	// or on): the provider follows the settings, as at every other place the
	// config is replaced.
	syncMicProvider(settings)
}

// relistMicSourcesCmd asks the daemon to have the other attached clients list
// their devices again. Fire-and-forget: the answers arrive over Watch, and a
// daemon that predates the RPC simply has no other clients to offer.
func relistMicSourcesCmd() tea.Cmd {
	return func() tea.Msg {
		_ = relistMicSourcesRemote()
		return nil
	}
}

// --- settings page --------------------------------------------------------------

// micClientName is this machine's name among the daemon's microphone clients
// (what its provider announces). A var so tests need not depend on the hostname.
var micClientName = mic.ClientName

// micChoice is one thing the selector can be set to: a client and a capture
// device on it. The zero value is "automatic" — no client selected, so whichever
// client attached most recently records, on its system default.
type micChoice struct {
	client string
	device string // "" = that client's system default
}

// micAttachedSource is client's entry in the daemon's pushed set, or nil if no
// provider is attached from it.
func micAttachedSource(m *model, client string) *fleetgrpc.MicSource {
	for _, source := range m.micSources.GetSources() {
		if source.GetClient() == client {
			return source
		}
	}
	return nil
}

// micCurrentSource is the client whose microphone the daemon records right now
// (which differs from the selection when that is automatic, or not attached).
func micCurrentSource(m *model) *fleetgrpc.MicSource {
	for _, source := range m.micSources.GetSources() {
		if source.GetSource() {
			return source
		}
	}
	return nil
}

// micClientDevices is what is known of client's capture devices; known=false
// means no listing has been seen. This machine's come from its own enumeration,
// which is authoritative and exists even while its provider is not attached;
// every other machine's come from the daemon.
func micClientDevices(m *model, client string) (devices []mic.Device, known bool) {
	if client == micClientName() && m.micDevicesLoaded {
		return m.micDevices, true
	}
	source := micAttachedSource(m, client)
	if source == nil || !source.GetDevicesListed() {
		return nil, false
	}
	for _, device := range source.GetDevices() {
		devices = append(devices, mic.Device{ID: device.GetId(), Label: device.GetLabel()})
	}
	return devices, true
}

// micChoices is everything the selector steps through: automatic, then this
// machine's microphones, then those of every other attached client.
func micChoices(m *model) []micChoice {
	self := micClientName()
	choices := []micChoice{{}}
	if m.micSources == nil {
		// A daemon that never pushed its clients predates selecting one: it
		// would drop the client from what we save, and the row would go on
		// showing a selection the daemon does not have. Offer what it can
		// store — a device, recorded by whichever client attached last.
		for _, device := range m.micDevices {
			choices = append(choices, micChoice{device: device.ID})
		}
		return choices
	}
	add := func(client string) {
		choices = append(choices, micChoice{client: client})
		devices, _ := micClientDevices(m, client)
		for _, device := range devices {
			choices = append(choices, micChoice{client: client, device: device.ID})
		}
	}
	// This machine is offered once it is known to be able to record at all: a
	// successful listing, or a provider of its own already attached.
	if m.micDevicesLoaded || micAttachedSource(m, self) != nil {
		add(self)
	}
	for _, source := range m.micSources.GetSources() {
		if source.GetClient() != self {
			add(source.GetClient())
		}
	}
	return choices
}

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

// cycleMicDevice steps the microphone through micChoices and saves. This
// machine's devices are enumerated lazily; a press before they have loaded
// starts that, and still steps through whatever the other clients offer.
func (settingsPage *settingsPage) cycleMicDevice(m *model, direction int) tea.Cmd {
	if m.config == nil {
		return nil
	}
	var load tea.Cmd
	if !m.micDevicesLoaded {
		load = m.ensureMicDevices()
	}
	choices := micChoices(m)
	if len(choices) == 1 {
		// Only "automatic". Either nothing has been listed yet — the listing just
		// started (or still running) is what will change that — or there really
		// is nothing else: a key press that does nothing looks broken, so say why.
		if m.micDevicesLoaded {
			m.message = "No selectable capture devices on this machine — recording the system default"
		}
		return load
	}
	settings := m.config.MicSettings
	current := micChoice{client: settings.Client, device: settings.Device}
	// A selection that is not among the choices (a client that is not attached,
	// a device that is gone) steps from the start of the list.
	index := max(slices.Index(choices, current), 0)
	next := choices[(index+direction+len(choices))%len(choices)]
	if next == current {
		return load
	}
	m.config.MicSettings.Client, m.config.MicSettings.Device = next.client, next.device
	if err := setConfigRemote(m.config); err != nil {
		m.config.MicSettings.Client, m.config.MicSettings.Device = current.client, current.device
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return load
	}
	m.message = fmt.Sprintf("Microphone set to %s", settingsPage.micDeviceLabel(m))
	// Moving the microphone to another client happens at once — the daemon tells
	// this machine to stop and the other to start. A different DEVICE on the
	// machine that is recording does not: a capture already running keeps its
	// device, so say so, or the message claims a switch that has not happened.
	staysHere := next.client == "" || next.client == micClientName()
	if m.micStatus.State == mic.StateLive && staysHere && next.device != current.device {
		m.message += " (from the next recording — this one keeps its device)"
	}
	return load
}

// micDeviceLabel names the configured microphone: the client, then the device
// on it. What cannot be found is shown for what it will do instead.
func (settingsPage *settingsPage) micDeviceLabel(m *model) string {
	settings := m.config.MicSettings
	self := micClientName()
	if settings.Client == "" {
		if settings.Device == "" {
			return "Automatic"
		}
		// A device with no client — a selection saved before clients could be
		// chosen. Whichever client records uses it if it has it; this machine's
		// listing is the one that can be shown.
		return "Automatic · " + micDeviceName(m, self, settings.Device)
	}
	name := settings.Client
	switch {
	case settings.Client == self:
		name += " (this machine)"
	case m.micSources != nil && micAttachedSource(m, settings.Client) == nil:
		name += " (not connected)"
	}
	return name + " · " + micDeviceName(m, settings.Client, settings.Device)
}

// micDeviceName names device on client. An id the client does not list is shown
// for what it will do: the provider falls back to its system default.
func micDeviceName(m *model, client, id string) string {
	if id == "" {
		return mic.DefaultLabel
	}
	devices, known := micClientDevices(m, client)
	for _, device := range devices {
		if device.ID == id {
			return device.Label
		}
	}
	if known {
		where := "there"
		if client == micClientName() {
			where = "here"
		}
		return mic.DefaultLabel + " (" + id + " not found " + where + ")"
	}
	return id
}

// micSourceNote is the dim line under the selector: where the audio actually
// comes from whenever that is not simply what the selection says.
func micSourceNote(m *model) string {
	settings := m.config.MicSettings
	self := micClientName()
	current := micCurrentSource(m)
	now := ""
	if current != nil {
		now = current.GetClient()
		if now == self {
			now += " (this machine)"
		}
	}
	switch {
	case m.micSources == nil:
		// Nothing pushed (yet, or ever: a daemon that predates the client list).
		return ""
	case settings.Client == "":
		if now == "" {
			return "follows the most recently connected client"
		}
		return "follows the most recently connected client — now " + now
	case micAttachedSource(m, settings.Client) == nil:
		if now == "" {
			return settings.Client + " is not connected"
		}
		return settings.Client + " is not connected — " + now + " records in its place"
	}
	return ""
}

// micDeviceValue renders the Source row's value.
func (settingsPage *settingsPage) micDeviceValue(m *model) string {
	if m.micDevicesLoading {
		return m.spinner.View() + " listing devices…"
	}
	indent := "\n" + strings.Repeat(" ", 21)
	value := fmt.Sprintf("[ %s ]", settingsPage.micDeviceLabel(m))
	if note := micSourceNote(m); note != "" {
		value += indent + dimStyle.Render(note)
	}
	if m.micDevicesErr != "" {
		value += indent + dimStyle.Render(m.micDevicesErr)
	}
	return value
}

// micStatusValue renders the (non-navigable) Status row from the provider's
// latest report.
func micStatusValue(m *model) string {
	indent := "\n" + strings.Repeat(" ", 21)
	switch m.micStatus.State {
	case mic.StateLive:
		value := statusRunningStyle.Render("● live") + "  " + dimStyle.Render("→ "+strings.Join(m.micStatus.Instances, ", "))
		if m.micStatus.FellBack {
			value += indent + dimStyle.Render("configured device not found here — recording the system default")
		}
		if m.micStatus.StandInFor != "" {
			value += indent + dimStyle.Render(m.micStatus.StandInFor+" is not connected — recording this machine's system default in its place")
		}
		return value
	case mic.StateIdle:
		// Idle because nothing records — or because another client is the source,
		// in which case this machine's microphone stays closed whatever happens.
		if current := micCurrentSource(m); current != nil && current.GetClient() != micClientName() {
			value := dimStyle.Render("standby — " + current.GetClient() + " is the microphone source")
			if current.GetRecording() {
				value += "  " + statusRunningStyle.Render("● live there")
			}
			return value
		}
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

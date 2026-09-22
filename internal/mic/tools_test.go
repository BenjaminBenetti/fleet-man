package mic

import (
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// argvOnly lets the argv assertions below read like assertions on a command.
type argvOnly struct{ Args []string }

// commandArgv resolves the recorder for deviceID without running anything.
func commandArgv(deviceID string) (*argvOnly, string, error) {
	argv, used, err := recorderArgv(deviceID)
	if err != nil {
		return nil, "", err
	}
	return &argvOnly{Args: argv}, used, nil
}

// fakeHost points detection at an imaginary machine: goos, the binaries on its
// PATH, and what each probe command prints.
func fakeHost(t *testing.T, os string, bins []string, probes map[string]string) {
	t.Helper()
	t.Setenv(EnvCapture, "")
	origOS, origLook, origProbe := goos, lookPath, runProbe
	goos = os
	lookPath = func(name string) (string, error) {
		if slices.Contains(bins, name) {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	runProbe = func(name string, args ...string) ([]byte, error) {
		out, ok := probes[name+" "+strings.Join(args, " ")]
		if !ok {
			return nil, errors.New("probe failed")
		}
		return []byte(out), nil
	}
	resetDetectCache()
	t.Cleanup(func() {
		goos, lookPath, runProbe = origOS, origLook, origProbe
		resetDetectCache()
	})
}

const pactlSources = `Source #0
	State: SUSPENDED
	Name: alsa_output.pci-0000_12_00.6.analog-stereo.monitor
	Description: Monitor of Family 17h HD Audio Analog Stereo
Source #1
	State: SUSPENDED
	Name: alsa_input.usb-Blue_Yeti-00.mono-fallback
	Description: Yeti Orb Mono
	Properties:
		device.description = "Yeti Orb"
Source #2
	State: RUNNING
	Name: bluez_input.AA_BB
`

func TestParsePactlSourcesSkipsMonitors(t *testing.T) {
	got := parsePactlSources(pactlSources)
	want := []Device{
		{ID: "pulse:alsa_input.usb-Blue_Yeti-00.mono-fallback", Label: "Yeti Orb Mono"},
		// No Description line: the name stands in for it.
		{ID: "pulse:bluez_input.AA_BB", Label: "bluez_input.AA_BB"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseArecordPCMsKeepsOnlyHardware(t *testing.T) {
	out := `null
    Discard all samples (playback) or generate zero samples (capture)
default
    Default Audio Device
plughw:CARD=Orb,DEV=0
    Yeti Orb, USB Audio
    Hardware device with all software conversions
sysdefault:CARD=Orb
    Yeti Orb, USB Audio
plughw:CARD=Generic,DEV=0
`
	got := parseArecordPCMs(out)
	want := []Device{
		{ID: "alsa:plughw:CARD=Orb,DEV=0", Label: "Yeti Orb, USB Audio"},
		{ID: "alsa:plughw:CARD=Generic,DEV=0", Label: "plughw:CARD=Generic,DEV=0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseAVFoundationAudioOnly(t *testing.T) {
	out := `[AVFoundation indev @ 0x7f8] AVFoundation video devices:
[AVFoundation indev @ 0x7f8] [0] FaceTime HD Camera
[AVFoundation indev @ 0x7f8] AVFoundation audio devices:
[AVFoundation indev @ 0x7f8] [0] MacBook Pro Microphone
[AVFoundation indev @ 0x7f8] [1] Yeti Orb
: Input/output error
`
	got := parseAVFoundationAudio(out)
	want := []Device{
		{ID: "avfoundation:MacBook Pro Microphone", Label: "MacBook Pro Microphone"},
		{ID: "avfoundation:Yeti Orb", Label: "Yeti Orb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestCommandPrefersPulseAndPassesTheDevice(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl", "arecord"}, map[string]string{
		"pactl info":         "ok",
		"pactl list sources": "Source #1\n\tName: yeti\n\tDescription: Yeti Orb\n",
	})
	cmd, used, err := commandArgv("pulse:yeti")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"parec", "--format=s16le", "--rate=16000", "--channels=1", "--device=yeti"} {
		if !strings.Contains(args, want) {
			t.Errorf("argv %q missing %q", args, want)
		}
	}
	if used != "pulse:yeti" {
		t.Errorf("used = %q, want the configured device", used)
	}
}

// A sound server that does not answer makes parec useless; fall through to ALSA.
func TestCommandSkipsPulseWithoutAServer(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl", "arecord"}, nil)
	cmd, _, err := commandArgv("")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := strings.Join(cmd.Args, " "); got != "arecord -q -t raw -f S16_LE -r 16000 -c 1 -" {
		t.Fatalf("argv = %q", got)
	}
}

// A device id minted by another machine's tool must not be handed to this one.
func TestCommandIgnoresAnotherToolsDevice(t *testing.T) {
	fakeHost(t, "linux", []string{"arecord"}, nil)
	cmd, used, err := commandArgv("avfoundation:MacBook Pro Microphone")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if slices.Contains(cmd.Args, "-D") || used != "" {
		t.Fatalf("foreign device leaked into argv %v (used %q)", cmd.Args, used)
	}
}

func TestCommandOnDarwinUsesAVFoundation(t *testing.T) {
	fakeHost(t, "darwin", []string{"ffmpeg", "arecord"}, map[string]string{
		"ffmpeg -hide_banner -f avfoundation -list_devices true -i ": "[AVFoundation indev @ 0x7f8] AVFoundation audio devices:\n[AVFoundation indev @ 0x7f8] [0] Yeti Orb\n",
	})
	cmd, _, err := commandArgv("avfoundation:Yeti Orb")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-f avfoundation -i :Yeti Orb") || !strings.Contains(args, "-f s16le") {
		t.Fatalf("argv = %q", args)
	}
}

func TestNoCaptureTool(t *testing.T) {
	fakeHost(t, "linux", nil, nil)
	if Available() {
		t.Fatal("Available() with nothing installed")
	}
	if _, _, err := commandArgv(""); !IsNoTool(err) {
		t.Fatalf("Command err = %v, want ErrNoCaptureTool", err)
	}
	if _, err := Devices(); !IsNoTool(err) {
		t.Fatalf("Devices err = %v, want ErrNoCaptureTool", err)
	}
}

func TestOverrideCommandWinsAndHasNoDevices(t *testing.T) {
	fakeHost(t, "linux", nil, nil)
	t.Setenv(EnvCapture, "cat /dev/zero")
	if !Available() {
		t.Fatal("the override alone must make capture available")
	}
	cmd, _, err := commandArgv("pulse:yeti")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := strings.Join(cmd.Args, " "); got != "sh -c cat /dev/zero" {
		t.Fatalf("argv = %q", got)
	}
	if devices, err := Devices(); err != nil || len(devices) != 0 {
		t.Fatalf("Devices = %v, %v; want none", devices, err)
	}
}

func TestDevicesListsThroughTheDetectedTool(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl"}, map[string]string{
		"pactl info":         "ok",
		"pactl list sources": pactlSources,
	})
	devices, err := Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 2 || devices[0].Label != "Yeti Orb Mono" {
		t.Fatalf("devices = %+v", devices)
	}
}

// Inside a fleet instance the "microphone" is fleet's own virtual one; offering
// it back to the daemon would be a self-sustaining loop. And the user must be
// told THAT — not to install packages they demonstrably already have.
func TestVirtualMicIsNeverOfferedAsAMicrophone(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl", "arecord"}, map[string]string{
		"pactl info": "Server Name: pulseaudio\nDefault Sink: fleetnull\nDefault Source: fleetmic\n",
	})
	err := Unavailable()
	if !errors.Is(err, ErrVirtualMic) {
		t.Fatalf("Unavailable = %v, want ErrVirtualMic", err)
	}
	if !IsNoTool(err) {
		t.Fatal("a virtual-mic machine must count as unable to capture (no retry)")
	}
	if got := Describe(err); strings.Contains(got, "install") || !strings.Contains(got, "fleet instance") {
		t.Fatalf("Describe = %q", got)
	}
	if _, _, err := commandArgv(""); !errors.Is(err, ErrVirtualMic) {
		t.Fatalf("Command err = %v, want ErrVirtualMic", err)
	}
}

// A real device that merely STARTS with the virtual mic's name is not it.
func TestVirtualMicMatchIsExact(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl"}, map[string]string{
		"pactl info": "Default Source: fleetmicrophone_usb\n",
	})
	if err := Unavailable(); err != nil {
		t.Fatalf("Unavailable = %v, want nil", err)
	}
}

// MicSettings.Device comes off the wire (a remote daemon's config). ALSA device
// strings are a small language in which some plugins take a FILENAME, so only a
// name this machine enumerated itself may reach `arecord -D`.
func TestCommandRejectsADeviceThisMachineDidNotEnumerate(t *testing.T) {
	fakeHost(t, "linux", []string{"arecord"}, map[string]string{
		"arecord -L": "plughw:CARD=Orb,DEV=0\n    Yeti Orb, USB Audio\n",
	})
	for _, hostile := range []string{
		"alsa:file:'/home/me/.ssh/authorized_keys',raw",
		"alsa:tee:default,'/tmp/x',raw",
		"alsa:plughw:CARD=Gone,DEV=0",
	} {
		cmd, used, err := commandArgv(hostile)
		if err != nil {
			t.Fatalf("Command(%q): %v", hostile, err)
		}
		if slices.Contains(cmd.Args, "-D") || used != "" {
			t.Fatalf("%q reached the recorder: argv %v (used %q)", hostile, cmd.Args, used)
		}
	}
	cmd, used, err := commandArgv("alsa:plughw:CARD=Orb,DEV=0")
	if err != nil || used != "alsa:plughw:CARD=Orb,DEV=0" || !slices.Contains(cmd.Args, "plughw:CARD=Orb,DEV=0") {
		t.Fatalf("an enumerated device must pass: argv %v used %q err %v", cmd.Args, used, err)
	}
}

// The fallback recorders nobody runs day to day are the ones most likely to be
// wrong; pin their argv and the order they are tried in.
func TestFallbackRecorders(t *testing.T) {
	fakeHost(t, "linux", []string{"ffmpeg", "rec"}, nil)
	cmd, _, err := commandArgv("")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := strings.Join(cmd.Args, " "); got != "ffmpeg -nostdin -hide_banner -loglevel error -fflags nobuffer -f alsa -i default -ac 1 -ar 16000 -f s16le -flush_packets 1 -" {
		t.Fatalf("linux ffmpeg argv = %q", got)
	}

	fakeHost(t, "linux", []string{"rec"}, nil)
	cmd, _, err = commandArgv("")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// -L: the wire format is little-endian by contract, not host-native.
	if got := strings.Join(cmd.Args, " "); got != "rec -q -t raw -r 16000 -e signed -b 16 -c 1 -L -" {
		t.Fatalf("sox argv = %q", got)
	}

	fakeHost(t, "darwin", []string{"rec", "arecord", "parec"}, nil)
	cmd, _, _ = commandArgv("")
	if cmd == nil || cmd.Args[0] != "rec" {
		t.Fatalf("darwin without ffmpeg should fall to sox, got %v", cmd)
	}
}

// One sound-server probe per detection: a wedged server costs probeTimeout on
// the capture-start path, so it must not be paid twice.
func TestDetectionProbesPulseOnce(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl"}, map[string]string{"pactl info": "ok"})
	calls := 0
	inner := runProbe
	runProbe = func(name string, args ...string) ([]byte, error) {
		if name == "pactl" && len(args) == 1 && args[0] == "info" {
			calls++
		}
		return inner(name, args...)
	}
	if !Available() {
		t.Fatal("expected pulse to be usable")
	}
	if calls != 1 {
		t.Fatalf("pactl info ran %d times, want 1", calls)
	}
}

func TestAVFoundationListWithoutATableIsAnError(t *testing.T) {
	fakeHost(t, "darwin", []string{"ffmpeg"}, map[string]string{
		"ffmpeg -hide_banner -f avfoundation -list_devices true -i ": "ffmpeg: unrecognized option\n",
	})
	if _, err := Devices(); err == nil {
		t.Fatal("no device table must be an error, not an empty list")
	}
}

// countListings wraps runProbe and counts device listings (not `pactl info`).
func countListings(t *testing.T) *int {
	t.Helper()
	calls := 0
	inner := runProbe
	runProbe = func(name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "info" {
			calls++
		}
		return inner(name, args...)
	}
	return &calls
}

// A stale device id — exactly the case the fallback exists for — must not cost
// an enumeration on every capture start.
func TestUnknownDeviceDoesNotRelistEveryTime(t *testing.T) {
	fakeHost(t, "linux", []string{"arecord"}, map[string]string{
		"arecord -L": "plughw:CARD=Orb,DEV=0\n    Yeti Orb\n",
	})
	listings := countListings(t)
	for range 5 {
		if _, used, _ := commandArgv("alsa:plughw:CARD=Gone,DEV=0"); used != "" {
			t.Fatal("an unknown device must not be used")
		}
	}
	if *listings != 1 {
		t.Fatalf("an unknown device listed devices %d times in a row, want 1", *listings)
	}
	// A KNOWN device costs nothing further.
	for range 3 {
		if _, used, _ := commandArgv("alsa:plughw:CARD=Orb,DEV=0"); used == "" {
			t.Fatal("a listed device must be used")
		}
	}
	if *listings != 1 {
		t.Fatalf("a known device re-listed (%d)", *listings)
	}
}

// What the settings page listed IS the allowlist: picking from it must not
// trigger a second enumeration at capture start.
func TestDevicesSeedsTheAllowlist(t *testing.T) {
	fakeHost(t, "linux", []string{"arecord"}, map[string]string{
		"arecord -L": "plughw:CARD=Orb,DEV=0\n    Yeti Orb\n",
	})
	listings := countListings(t)
	if _, err := Devices(); err != nil {
		t.Fatal(err)
	}
	if _, used, _ := commandArgv("alsa:plughw:CARD=Orb,DEV=0"); used == "" {
		t.Fatal("a device Devices() returned must validate")
	}
	if *listings != 1 {
		t.Fatalf("listed %d times, want 1", *listings)
	}
}

// The allowlist holds FULL ids and is tied to the detection verdict it was made
// under: a name pulse enumerated must never validate for ALSA — not even when a
// slow pulse listing lands after detection has moved on to arecord.
func TestAllowlistCannotCrossTools(t *testing.T) {
	const name = "file:'/tmp/pwn',raw" // a pulse SOURCE may be named anything
	fakeHost(t, "linux", []string{"parec", "pactl", "arecord"}, map[string]string{
		"pactl info":         "ok",
		"pactl list sources": "Source #1\n\tName: " + name + "\n",
		"arecord -L":         "default\n",
	})
	if _, used, _ := commandArgv("pulse:" + name); used == "" {
		t.Fatal("setup: pulse should accept its own source")
	}

	// Same cache, no re-detection: the prefix alone must not get it through.
	if cmd, used, _ := commandArgv("alsa:" + name); used != "" || slices.Contains(cmd.Args, "-D") {
		t.Fatalf("a pulse name validated under the alsa prefix: %v", cmd.Args)
	}

	// The race: a pulse listing still in flight when detection switches to alsa.
	detectCache.mu.Lock()
	staleEpoch := detectCache.epoch
	detectCache.mu.Unlock()
	lookPath = func(bin string) (string, error) {
		if bin == "arecord" {
			return "/usr/bin/arecord", nil
		}
		return "", exec.ErrNotFound
	}
	resetDetectCache()
	if detected, err := detect(); err != nil || detected.name != "alsa" {
		t.Fatalf("setup: expected alsa, got %v %v", detected.name, err)
	}
	storeDevices(staleEpoch, []Device{{ID: "alsa:" + name}}) // the late pulse-era write
	if cmd, used, _ := commandArgv("alsa:" + name); used != "" || slices.Contains(cmd.Args, "-D") {
		t.Fatalf("a listing from a superseded detection was stored: %v", cmd.Args)
	}
}

// A listing that FAILS says nothing about the hardware: it must not wipe a good
// allowlist and send the user to a different microphone because the sound server
// was busy for a moment.
func TestFailedListingKeepsThePreviousAllowlist(t *testing.T) {
	probes := map[string]string{"arecord -L": "plughw:CARD=Orb,DEV=0\n    Yeti Orb\n"}
	fakeHost(t, "linux", []string{"arecord"}, probes)
	if _, used, _ := commandArgv("alsa:plughw:CARD=Orb,DEV=0"); used == "" {
		t.Fatal("setup: the device should validate")
	}

	delete(probes, "arecord -L") // the sound server is momentarily wedged
	// Age the listing past deviceListTTL, so the miss below really re-lists
	// (and that re-list is the one that fails).
	detectCache.mu.Lock()
	detectCache.devicesAt = time.Now().Add(-2 * deviceListTTL)
	detectCache.mu.Unlock()
	listings := countListings(t)
	if _, used, _ := commandArgv("alsa:plughw:CARD=Elsewhere,DEV=0"); used != "" {
		t.Fatal("an unknown id must still be refused") // …and this miss triggers the failing re-list
	}
	if *listings != 1 {
		t.Fatalf("setup: expected the miss to trigger exactly one (failing) re-list, got %d", *listings)
	}
	if _, used, _ := commandArgv("alsa:plughw:CARD=Orb,DEV=0"); used == "" {
		t.Fatal("a failed re-list wiped the allowlist: the configured, working device fell back to the default")
	}
	// …and the failure is rate-limited like any other listing.
	_, _, _ = commandArgv("alsa:plughw:CARD=Elsewhere,DEV=0")
	if *listings != 1 {
		t.Fatalf("a wedged server was re-probed at once (%d listings)", *listings)
	}
}

// With NO earlier listing, a failing one must still be rate-limited: a wedged
// sound server probed on every capture start costs seconds before the microphone
// opens — the start of every sentence.
func TestFailedFirstListingIsStillRateLimited(t *testing.T) {
	fakeHost(t, "linux", []string{"arecord"}, nil) // arecord -L fails
	listings := countListings(t)
	for range 4 {
		if _, used, _ := commandArgv("alsa:plughw:CARD=Orb,DEV=0"); used != "" {
			t.Fatal("an unvalidated id must not be used")
		}
	}
	if *listings != 1 {
		t.Fatalf("a failing listing ran %d times for 4 capture starts, want 1", *listings)
	}
}

// Opening the device selector re-detects the tool, but a listing that then FAILS
// must not leave capture with no allowlist at all.
func TestDevicesFailureKeepsTheAllowlist(t *testing.T) {
	probes := map[string]string{"arecord -L": "plughw:CARD=Orb,DEV=0\n    Yeti Orb\n"}
	fakeHost(t, "linux", []string{"arecord"}, probes)
	if _, err := Devices(); err != nil {
		t.Fatal(err)
	}
	delete(probes, "arecord -L")
	if _, err := Devices(); err == nil {
		t.Fatal("setup: the second listing should fail")
	}
	if _, used, _ := commandArgv("alsa:plughw:CARD=Orb,DEV=0"); used == "" {
		t.Fatal("a failed Devices() wiped the allowlist: the configured device fell back to the default")
	}
}

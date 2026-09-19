package mic

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
)

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
	cmd, used, err := Command(context.Background(), "pulse:yeti")
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
	cmd, _, err := Command(context.Background(), "")
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
	cmd, used, err := Command(context.Background(), "avfoundation:MacBook Pro Microphone")
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
	cmd, _, err := Command(context.Background(), "avfoundation:Yeti Orb")
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
	if _, _, err := Command(context.Background(), ""); !IsNoTool(err) {
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
	cmd, _, err := Command(context.Background(), "pulse:yeti")
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
	if _, _, err := Command(context.Background(), ""); !errors.Is(err, ErrVirtualMic) {
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
		cmd, used, err := Command(context.Background(), hostile)
		if err != nil {
			t.Fatalf("Command(%q): %v", hostile, err)
		}
		if slices.Contains(cmd.Args, "-D") || used != "" {
			t.Fatalf("%q reached the recorder: argv %v (used %q)", hostile, cmd.Args, used)
		}
	}
	cmd, used, err := Command(context.Background(), "alsa:plughw:CARD=Orb,DEV=0")
	if err != nil || used != "alsa:plughw:CARD=Orb,DEV=0" || !slices.Contains(cmd.Args, "plughw:CARD=Orb,DEV=0") {
		t.Fatalf("an enumerated device must pass: argv %v used %q err %v", cmd.Args, used, err)
	}
}

// The fallback recorders nobody runs day to day are the ones most likely to be
// wrong; pin their argv and the order they are tried in.
func TestFallbackRecorders(t *testing.T) {
	fakeHost(t, "linux", []string{"ffmpeg", "rec"}, nil)
	cmd, _, err := Command(context.Background(), "")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := strings.Join(cmd.Args, " "); got != "ffmpeg -nostdin -hide_banner -loglevel error -fflags nobuffer -f alsa -i default -ac 1 -ar 16000 -f s16le -flush_packets 1 -" {
		t.Fatalf("linux ffmpeg argv = %q", got)
	}

	fakeHost(t, "linux", []string{"rec"}, nil)
	cmd, _, err = Command(context.Background(), "")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// -L: the wire format is little-endian by contract, not host-native.
	if got := strings.Join(cmd.Args, " "); got != "rec -q -t raw -r 16000 -e signed -b 16 -c 1 -L -" {
		t.Fatalf("sox argv = %q", got)
	}

	fakeHost(t, "darwin", []string{"rec", "arecord", "parec"}, nil)
	cmd, _, _ = Command(context.Background(), "")
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

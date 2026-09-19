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
	fakeHost(t, "linux", []string{"parec", "pactl", "arecord"}, map[string]string{"pactl info": "ok"})
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
	fakeHost(t, "darwin", []string{"ffmpeg", "arecord"}, nil)
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
// it back to the daemon would be a self-sustaining loop.
func TestVirtualMicIsNeverOfferedAsAMicrophone(t *testing.T) {
	fakeHost(t, "linux", []string{"parec", "pactl", "arecord"}, map[string]string{
		"pactl info": "Server Name: pulseaudio\nDefault Sink: fleetnull\nDefault Source: fleetmic\n",
	})
	if Available() {
		t.Fatal("a machine whose default source is the fleet virtual mic has no microphone to offer")
	}
	if _, _, err := Command(context.Background(), ""); !IsNoTool(err) {
		t.Fatalf("Command err = %v, want ErrNoCaptureTool", err)
	}
}

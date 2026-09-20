package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"google.golang.org/grpc"
)

// The in-instance plumbing must stay out of --help; the two commands a person
// would run must be there.
func TestMicCommandSurface(t *testing.T) {
	visible := map[string]bool{}
	for _, sub := range newMicCmd().Commands() {
		visible[sub.Name()] = !sub.Hidden
	}
	for name, want := range map[string]bool{"devices": true, "attach": true, "sink": false, "ensure": false, "stop": false} {
		got, ok := visible[name]
		if !ok {
			t.Errorf("fleet mic %s is missing", name)
		} else if got != want {
			t.Errorf("fleet mic %s visible = %v, want %v", name, got, want)
		}
	}
}

// With an override recorder there is nothing to enumerate: the listing is just
// the implicit default, and it is not an error.
func TestMicDevicesListsTheDefault(t *testing.T) {
	t.Setenv(mic.EnvCapture, "cat /dev/zero")
	cmd := newMicCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"devices"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fleet mic devices: %v", err)
	}
	if !strings.Contains(out.String(), "(default)") || !strings.Contains(out.String(), mic.DefaultLabel) {
		t.Fatalf("output = %q", out.String())
	}
}

// configClient is a FleetServiceClient that only answers GetConfig.
type configClient struct {
	fleetgrpc.FleetServiceClient
	device string
	err    error
	calls  int
}

func (c *configClient) GetConfig(context.Context, *fleetgrpc.GetConfigRequest, ...grpc.CallOption) (*fleetgrpc.GetConfigReply, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &fleetgrpc.GetConfigReply{Config: &fleetgrpc.Config{Mic: &fleetgrpc.MicSettings{Enabled: true, Device: c.device}}}, nil
}

// `fleet mic attach` tells the user to pick the device in Settings; it must then
// record from THAT device — as an open TUI does — not silently from the system
// default. And, like the TUI, a selection changed while it runs applies to the
// next recording.
func TestAttachRecordsFromTheDeviceChosenInSettings(t *testing.T) {
	daemon := &configClient{device: "pulse:desk_mic"}
	device := micDeviceResolver(context.Background(), daemon, "")

	if got := device(); got != "pulse:desk_mic" {
		t.Fatalf("device = %q, want the one configured in Settings", got)
	}
	daemon.device = "pulse:headset"
	if got := device(); got != "pulse:headset" {
		t.Fatalf("device = %q: a changed selection must apply to the next recording", got)
	}
	daemon.device = ""
	if got := device(); got != "" {
		t.Fatalf("device = %q: switching back to the system default must apply too", got)
	}
}

// A daemon that cannot be asked in time must not flip the recording to the
// system default: the last known selection stands.
func TestAttachKeepsTheLastKnownDeviceWhenTheDaemonIsSlow(t *testing.T) {
	daemon := &configClient{device: "pulse:desk_mic"}
	device := micDeviceResolver(context.Background(), daemon, "")
	_ = device()
	daemon.err = errors.New("deadline exceeded")
	if got := device(); got != "pulse:desk_mic" {
		t.Fatalf("device = %q, want the last known selection", got)
	}
}

func TestAttachDeviceFlagOverridesSettings(t *testing.T) {
	daemon := &configClient{device: "pulse:desk_mic"}
	device := micDeviceResolver(context.Background(), daemon, "pulse:from_flag")
	if got := device(); got != "pulse:from_flag" {
		t.Fatalf("device = %q, want the flag", got)
	}
	if daemon.calls != 0 {
		t.Fatal("an explicit --device needs no config lookup")
	}
}

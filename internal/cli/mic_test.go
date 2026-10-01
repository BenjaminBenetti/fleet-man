package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
)

// The in-instance plumbing must stay out of --help; the two commands a person
// would run must be there.
func TestMicCommandSurface(t *testing.T) {
	visible := map[string]bool{}
	for _, sub := range newMicCmd().Commands() {
		visible[sub.Name()] = !sub.Hidden
	}
	for name, want := range map[string]bool{"devices": true, "sources": true, "attach": true, "sink": false, "ensure": false, "stop": false} {
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

// `fleet mic sources` is the selector's list for a terminal: every client, its
// implicit default and its devices, with the recorded client marked.
func TestPrintMicSources(t *testing.T) {
	var out bytes.Buffer
	printMicSources(&out, &fleetgrpc.MicSources{})
	if !strings.Contains(out.String(), "no microphone clients connected") {
		t.Fatalf("empty output = %q", out.String())
	}

	out.Reset()
	printMicSources(&out, &fleetgrpc.MicSources{Sources: []*fleetgrpc.MicSource{
		{Client: "desk", Source: true, DevicesListed: true, Devices: []*fleetgrpc.MicDevice{{Id: "pulse:usb", Label: "USB Mic"}}},
		{Client: "a-much-longer-laptop-name"},
	}})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and three rows, got:\n%s", out.String())
	}
	for i, want := range [][]string{
		{"CLIENT", "DEVICE", "LABEL"},
		{"* desk", "(default)", mic.DefaultLabel},
		{"* desk", "pulse:usb", "USB Mic"},
		{"  a-much-longer-laptop-name", "(default)", mic.DefaultLabel},
	} {
		for _, field := range want {
			if !strings.Contains(lines[i], field) {
				t.Errorf("line %d = %q, want it to contain %q", i, lines[i], field)
			}
		}
	}
	// Columns line up under the longest client name.
	if strings.Index(lines[1], "(default)") != strings.Index(lines[3], "(default)") {
		t.Errorf("device column is not aligned:\n%s", out.String())
	}
}

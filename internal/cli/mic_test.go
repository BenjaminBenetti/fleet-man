package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/mic"
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

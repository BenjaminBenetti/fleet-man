package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// runMount runs `fleet mount` args against settings held in memory, and
// returns stdout, stderr and the settings after.
func runMount(t *testing.T, seed fleet.FleetSettings, warnings []string, args ...string) (string, string, fleet.FleetSettings, error) {
	t.Helper()
	held := seed
	origLoad, origMutate := loadMounts, mutateMounts
	t.Cleanup(func() { loadMounts, mutateMounts = origLoad, origMutate })
	loadMounts = func(_ context.Context, fleetName string) (fleet.FleetSettings, error) {
		if fleetName != "alpha" {
			t.Errorf("fleet %q", fleetName)
		}
		return held, nil
	}
	mutateMounts = func(_ context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) ([]string, error) {
		out, err := fn(held)
		if err != nil {
			return nil, err
		}
		held = out
		return warnings, nil
	}
	cmd := NewRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"mount"}, args...))
	err := cmd.Execute()
	return stdout.String(), stderr.String(), held, err
}

func TestMountCommands(t *testing.T) {
	out, stderr, s, err := runMount(t, fleet.FleetSettings{CustomMounts: []string{"/srv"}}, []string{"custom mount /opt/data: host folder /big does not exist on the daemon host; create it before making an instance"},
		"add", "alpha", "/opt/data/", "--host", "/big/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Mounted /opt/data") || s.CustomMountHostFolders["/opt/data"] != "/big" || len(s.CustomMounts) != 2 {
		t.Errorf("add: %q, %+v", out, s)
	}
	if !strings.HasPrefix(stderr, "warning: custom mount /opt/data") {
		t.Errorf("the daemon's warning: %q", stderr)
	}

	out, _, _, err = runMount(t, s, nil, "ls", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PATH", "HOST", "/srv", "-", "/opt/data", "/big"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}

	// Without --host the mount goes back to its .mnt folder.
	_, _, s, err = runMount(t, s, nil, "add", "alpha", "/opt/data")
	if err != nil || len(s.CustomMountHostFolders) != 0 {
		t.Errorf("re-add without --host: %+v, %v", s, err)
	}
	_, _, s, err = runMount(t, s, nil, "rm", "alpha", "/srv")
	if err != nil || len(s.CustomMounts) != 1 || s.CustomMounts[0] != "/opt/data" {
		t.Errorf("rm: %+v, %v", s, err)
	}
	if _, _, _, err := runMount(t, s, nil, "add", "alpha", "/x", "--host", "relative"); err == nil {
		t.Error("a relative host folder was accepted")
	}
	if _, _, _, err := runMount(t, s, nil, "rm", "alpha", "/nope"); err == nil {
		t.Error("removing a mount the fleet does not have succeeded")
	}
}

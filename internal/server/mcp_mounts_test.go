package server

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestMCPMountLifecycle: the mount tools add a custom mount, give it a host
// folder (warning when the daemon host lacks it), take it back to its .mnt
// folder and remove it, each through SetFleetSettings' checks, every other
// setting kept.
func TestMCPMountLifecycle(t *testing.T) {
	cs := seedBareFleet(t)
	var out MountsOutput
	callJSON(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "keep-me"}, &AutomationOutput{})

	callJSON(t, cs, "fleet_mount_set", map[string]any{"fleet": "alpha", "path": "/opt/data/"}, &out)
	if len(out.Mounts) != 1 || out.Mounts[0].Path != "/opt/data" || out.Mounts[0].HostFolder != "" {
		t.Fatalf("set returned %+v", out)
	}
	big := t.TempDir()
	callJSON(t, cs, "fleet_mount_set", map[string]any{"fleet": "alpha", "path": "/opt/data", "host_folder": big + "/"}, &out)
	if len(out.Mounts) != 1 || out.Mounts[0].HostFolder != big || len(out.Warnings) != 0 {
		t.Fatalf("set with a host folder returned %+v", out)
	}
	callJSON(t, cs, "fleet_mount_set", map[string]any{"fleet": "alpha", "path": "/srv", "host_folder": big + "/missing"}, &out)
	if len(out.Mounts) != 2 || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "does not exist on the daemon host") {
		t.Fatalf("set with a missing host folder returned %+v", out)
	}
	var listed MountsOutput
	callJSON(t, cs, "fleet_mount_list", map[string]any{"fleet": "alpha"}, &listed)
	if len(listed.Mounts) != 2 || listed.Mounts[0].HostFolder != big {
		t.Fatalf("list returned %+v", listed)
	}
	if msg := callErr(t, cs, "fleet_mount_set", map[string]any{"fleet": "alpha", "path": "/x", "host_folder": state.FleetDir()}); !strings.Contains(msg, "fleet's own state") {
		t.Errorf("a host folder over fleet's state: %q", msg)
	}
	out = MountsOutput{}
	callJSON(t, cs, "fleet_mount_set", map[string]any{"fleet": "alpha", "path": "/opt/data"}, &out)
	if out.Mounts[0].HostFolder != "" {
		t.Errorf("set without host_folder kept it: %+v", out)
	}
	out = MountsOutput{}
	callJSON(t, cs, "fleet_mount_delete", map[string]any{"fleet": "alpha", "path": "/srv"}, &out)
	if len(out.Mounts) != 1 || out.Mounts[0].Path != "/opt/data" {
		t.Fatalf("delete returned %+v", out)
	}
	if msg := callErr(t, cs, "fleet_mount_delete", map[string]any{"fleet": "alpha", "path": "/nope"}); !strings.Contains(msg, "no custom mount") {
		t.Errorf("deleting a mount the fleet lacks: %q", msg)
	}
	st, _ := state.Load()
	s := st.Fleets["alpha"].Settings
	if len(s.Agents) != 1 || len(s.CustomMountHostFolders) != 0 || len(s.CustomMounts) != 1 {
		t.Errorf("persisted settings %+v", s)
	}
}

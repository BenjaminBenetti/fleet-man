package server

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// everySetting is a settings object with every option of the Edit fleet
// dialog set (none at its zero value), with hostFolder as the custom mount's
// host folder.
func everySetting(hostFolder string) map[string]any {
	return map[string]any{
		"claude_code_mount":   true,
		"codex_mount":         true,
		"auggie_mount":        true,
		"fleet_mcp":           true,
		"gh_mount":            true,
		"home_dir":            "/home/node/",
		"prefer_fleet_launch": true,
		"layout_presets": []any{
			map[string]any{"name": "dev", "pane_commands": []string{"npm run dev", ""}, "layout": "b25d,80x24,0,0{40x24,0,0,0,39x24,41,0[39x12,41,0,1,39x11,41,13,2]}"},
		},
		"custom_mounts": []any{
			map[string]any{"path": "/opt/data/"},
			map[string]any{"path": "/srv/big", "host_folder": hostFolder},
		},
		"buildkit_server":      true,
		"deb_cache_server":     true,
		"image_cache_server":   true,
		"coder_workspace_name": "Web",
		"coder_template":       "docker",
		"coder_preset":         "large",
		"coder_parameters":     []any{map[string]any{"name": "region", "value": "eu"}},
	}
}

// fleetSettingsOf is the persisted settings of fleet name.
func fleetSettingsOf(t *testing.T, name string) fleet.FleetSettings {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	f, ok := st.Fleets[name]
	if !ok {
		t.Fatalf("fleet %q not in state", name)
	}
	return f.Settings
}

// TestMCPFleetSettingsCoverTheDialog: the MCP settings object reaches every
// FleetSettings field but the automation lists (their own tools edit them),
// so a setting added to the Edit fleet dialog fails here until fleet_up and
// fleet_edit can set it too.
func TestMCPFleetSettingsCoverTheDialog(t *testing.T) {
	raw, _ := json.Marshal(everySetting("/mnt/big"))
	var in mcpFleetSettingsInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := applyFleetSettings(fleet.FleetSettings{}, &in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	ownTools := map[string]bool{"Agents": true, "Triggers": true}
	v := reflect.ValueOf(got)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if !ownTools[name] && v.Field(i).IsZero() {
			t.Errorf("FleetSettings.%s is not settable through the MCP settings object", name)
		}
	}
}

// TestMCPFleetEditSetsEverySetting: fleet_edit sets every option, through
// SetFleetSettings' normalization, keeping the automation, and
// fleet_settings reads the same back.
func TestMCPFleetEditSetsEverySetting(t *testing.T) {
	cs := seedBareFleet(t)
	callJSON(t, cs, "fleet_agent_create", map[string]any{"fleet": "alpha", "name": "keep-me"}, &AutomationOutput{})
	big := t.TempDir()

	var out FleetSettingsOutput
	callJSON(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": everySetting(big)}, &out)
	if out.Fleet != "alpha" || out.Remote != "git@example.com:a.git" || len(out.Warnings) != 0 {
		t.Fatalf("edit returned %+v", out)
	}
	got := out.Settings
	if !got.ClaudeCodeMount || !got.CodexMount || !got.AuggieMount || !got.FleetMCP || !got.GhMount ||
		!got.BuildkitServer || !got.DebCacheServer || !got.ImageCacheServer {
		t.Errorf("toggles not all on: %+v", got)
	}
	if got.HomeDir != "/home/node" {
		t.Errorf("home_dir = %q", got.HomeDir)
	}
	if got.PreferFleetLaunch == nil || !*got.PreferFleetLaunch {
		t.Errorf("prefer_fleet_launch = %v", got.PreferFleetLaunch)
	}
	if len(got.LayoutPresets) != 1 || got.LayoutPresets[0].Name != "dev" || len(got.LayoutPresets[0].PaneCommands) != 2 || got.LayoutPresets[0].Layout == "" {
		t.Errorf("layout_presets = %+v", got.LayoutPresets)
	}
	if len(got.CustomMounts) != 2 || got.CustomMounts[0] != (mcpMount{Path: "/opt/data"}) || got.CustomMounts[1] != (mcpMount{Path: "/srv/big", HostFolder: big}) {
		t.Errorf("custom_mounts = %+v", got.CustomMounts)
	}
	if got.CoderWorkspaceName != "web" || got.CoderTemplate != "docker" || got.CoderPreset != "large" ||
		len(got.CoderParameters) != 1 || got.CoderParameters[0].Name != "region" || got.CoderParameters[0].Value != "eu" {
		t.Errorf("coder settings = %+v", got)
	}

	s := fleetSettingsOf(t, "alpha")
	if len(s.Agents) != 1 || s.CustomMountHostFolders["/srv/big"] != big || s.CoderWorkspaceName != "web" {
		t.Errorf("persisted settings %+v", s)
	}
	var read FleetSettingsOutput
	callJSON(t, cs, "fleet_settings", map[string]any{"fleet": "alpha"}, &read)
	if !reflect.DeepEqual(read.Settings, out.Settings) {
		t.Errorf("fleet_settings = %+v, edit returned %+v", read.Settings, out.Settings)
	}
}

// TestMCPFleetEditChangesOnlyWhatIsPassed: an edit merges — an omitted
// setting keeps its value, false and "" are values, [] empties a list —
// and an omitted preset layout survives while its pane count does.
func TestMCPFleetEditChangesOnlyWhatIsPassed(t *testing.T) {
	cs := seedBareFleet(t)
	callJSON(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": everySetting(t.TempDir())}, &FleetSettingsOutput{})

	var out FleetSettingsOutput
	callJSON(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": map[string]any{
		"gh_mount":            false,
		"prefer_fleet_launch": false,
		"home_dir":            "",
		"custom_mounts":       []any{},
		"layout_presets":      []any{map[string]any{"name": "dev", "pane_commands": []string{"make watch", "htop"}}},
	}}, &out)
	got := out.Settings
	if got.GhMount || got.PreferFleetLaunch == nil || *got.PreferFleetLaunch || got.HomeDir != "" || len(got.CustomMounts) != 0 {
		t.Errorf("passed settings not applied: %+v", got)
	}
	if !got.ClaudeCodeMount || !got.FleetMCP || !got.ImageCacheServer || got.CoderTemplate != "docker" || len(got.CoderParameters) != 1 {
		t.Errorf("omitted settings changed: %+v", got)
	}
	if p := got.LayoutPresets; len(p) != 1 || p[0].PaneCommands[0] != "make watch" || p[0].Layout == "" {
		t.Errorf("same-pane-count preset lost its layout: %+v", p)
	}
	var resized FleetSettingsOutput
	callJSON(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": map[string]any{
		"layout_presets": []any{map[string]any{"name": "dev", "pane_commands": []string{"a", "b", "c"}}},
	}}, &resized)
	if p := resized.Settings.LayoutPresets; len(p) != 1 || p[0].Layout != "" {
		t.Errorf("a preset with another pane count kept the old layout: %+v", p)
	}

	// No template: nothing to preset or bind, as in the TUI.
	var cleared FleetSettingsOutput
	callJSON(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": map[string]any{"coder_template": ""}}, &cleared)
	if c := cleared.Settings; c.CoderTemplate != "" || c.CoderPreset != "" || len(c.CoderParameters) != 0 {
		t.Errorf("clearing the template kept its preset or parameters: %+v", c)
	}
	if cleared.Settings.CoderWorkspaceName != "web" {
		t.Errorf("clearing the template cleared the workspace name: %+v", cleared.Settings)
	}
}

// TestMCPFleetEditRefuses: a bad setting is a tool error that changes
// nothing.
func TestMCPFleetEditRefuses(t *testing.T) {
	cs := seedBareFleet(t)
	callJSON(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": map[string]any{"codex_mount": true}}, &FleetSettingsOutput{})
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{"relative home dir", map[string]any{"home_dir": "~/"}, "absolute"},
		{"relative mount", map[string]any{"custom_mounts": []any{map[string]any{"path": "data"}}}, "absolute"},
		{"mount over fleet's state", map[string]any{"custom_mounts": []any{map[string]any{"path": "/x", "host_folder": state.FleetDir()}}}, "fleet's own state"},
		{"duplicate preset", map[string]any{"layout_presets": []any{
			map[string]any{"name": "a", "pane_commands": []string{""}},
			map[string]any{"name": "a", "pane_commands": []string{""}},
		}}, "duplicate"},
		{"pane-less preset", map[string]any{"layout_presets": []any{map[string]any{"name": "a", "pane_commands": []string{}}}}, "no panes"},
		{"preset without template", map[string]any{"coder_preset": "large"}, "need a coder_template"},
		{"duplicate parameter", map[string]any{"coder_template": "t", "coder_parameters": []any{
			map[string]any{"name": "r", "value": "1"}, map[string]any{"name": "r", "value": "2"},
		}}, "twice"},
		{"bad workspace name", map[string]any{"coder_workspace_name": "no spaces"}, "coder workspace name"},
		{"unknown setting", map[string]any{"claude_mount": true}, "claude_mount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if msg := callErr(t, cs, "fleet_edit", map[string]any{"fleet": "alpha", "settings": tc.settings}); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q, want it to mention %q", msg, tc.want)
			}
		})
	}
	if s := fleetSettingsOf(t, "alpha"); !s.CodexMount || s.HomeDir != "" || len(s.CustomMounts) != 0 || len(s.LayoutPresets) != 0 || s.CoderPreset != "" {
		t.Errorf("a refused edit changed the settings: %+v", s)
	}
	if msg := callErr(t, cs, "fleet_edit", map[string]any{"fleet": "ghost", "settings": map[string]any{"gh_mount": true}}); !strings.Contains(msg, "not found") {
		t.Errorf("unknown fleet: %q", msg)
	}
	if msg := callErr(t, cs, "fleet_settings", map[string]any{"fleet": "ghost"}); !strings.Contains(msg, "not found") {
		t.Errorf("unknown fleet read: %q", msg)
	}
}

// TestMCPUpMakesTheFleetWithSettings: fleet_up's settings are on the new
// fleet before its first instance provisions; an existing fleet, a bad
// setting and a refused create are tool errors that leave state alone.
func TestMCPUpMakesTheFleetWithSettings(t *testing.T) {
	isolateFleetDir(t)
	provisioned := make(chan fleet.FleetSettings, 1)
	orig := jobRunCreate
	jobRunCreate = func(_ context.Context, fleetName, instanceName, _, _ string, _ bool, _ fleet.BackendType) error {
		return state.Update(func(st *state.State) error {
			provisioned <- st.Fleets[fleetName].Settings
			inst, _ := st.Fleets[fleetName].GetInstance(instanceName)
			inst.Status = fleet.StatusRunning
			return nil
		})
	}
	defer func() { jobRunCreate = orig }()
	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)
	big := t.TempDir()

	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{
		"fleet": "web", "instance": "i1", "remote": "git@example.com:web.git", "backend": "devcontainer",
		"settings": everySetting(big + "/missing"), "wait": true,
	}, &out)
	if !out.Success || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "does not exist on the daemon host") {
		t.Fatalf("fleet_up returned %+v", out)
	}
	s := <-provisioned
	if !s.ClaudeCodeMount || !s.FleetMCP || !s.DebCacheServer || s.HomeDir != "/home/node" || len(s.CustomMounts) != 2 ||
		len(s.LayoutPresets) != 1 || s.CoderWorkspaceName != "web" || s.PreferFleetLaunch == nil {
		t.Errorf("the first instance provisioned with settings %+v", s)
	}

	if msg := callErr(t, cs, "fleet_up", map[string]any{"fleet": "web", "instance": "i2", "settings": map[string]any{"gh_mount": false}}); !strings.Contains(msg, "already exists") || !strings.Contains(msg, "fleet_edit") {
		t.Errorf("settings for an existing fleet: %q", msg)
	}
	if msg := callErr(t, cs, "fleet_up", map[string]any{"fleet": "bad", "instance": "i1", "remote": "git@example.com:bad.git", "settings": map[string]any{"home_dir": "home"}}); !strings.Contains(msg, "absolute") {
		t.Errorf("a bad setting: %q", msg)
	}
	if msg := callErr(t, cs, "fleet_up", map[string]any{"fleet": "tpl", "instance": "i1", "remote": "file://" + big + "/nope", "settings": map[string]any{"gh_mount": true}}); msg == "" {
		t.Error("a refused create returned no error")
	}
	st, _ := state.Load()
	if len(st.Fleets) != 1 || len(st.Fleets["web"].Instances) != 1 || !st.Fleets["web"].Settings.GhMount {
		t.Errorf("refused creates changed state: %d fleets, web %+v", len(st.Fleets), st.Fleets["web"])
	}
}

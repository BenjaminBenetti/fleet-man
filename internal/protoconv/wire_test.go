package protoconv

import (
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The round-trip suite (protoconv_test.go) guards field PRESENCE but is blind
// to symmetric encode/decode swaps: a converter pair that writes Machine into
// idle_timeout and reads it back from idle_timeout round-trips cleanly. These
// tests pin wire PLACEMENT by asserting proto getters directly, so the wire
// contract stays correct for cross-version client/server pairs — one distinct
// value per string field, and a set-one-assert-its-getter loop for bools
// (whose two values can't be made mutually distinct).

func TestTriggerWirePlacement(t *testing.T) {
	pt := TriggersToProto([]fleet.Trigger{{
		Name:        "nm",
		Type:        fleet.TriggerWebhook,
		AgentNames:  []string{"a1", "a2"},
		Prompt:      "pr",
		Cron:        "cr",
		Script:      "sc",
		WebhookName: "wh",
		FilterType:  fleet.WebhookFilterJSONPath,
		Regex:       "rx",
		JSONPath:    "jp",
		JSONValue:   "jv",
		Disabled:    true,
	}})[0]
	if pt.GetName() != "nm" || pt.GetType() != "webhook" || pt.GetPrompt() != "pr" ||
		pt.GetCron() != "cr" || pt.GetScript() != "sc" || pt.GetWebhookName() != "wh" ||
		pt.GetFilterType() != "jsonpath" || pt.GetRegex() != "rx" ||
		pt.GetJsonPath() != "jp" || pt.GetJsonValue() != "jv" || !pt.GetDisabled() ||
		len(pt.GetAgentNames()) != 2 || pt.GetAgentNames()[0] != "a1" {
		t.Fatalf("trigger wire placement wrong: %+v", pt)
	}
}

func TestAgentWirePlacement(t *testing.T) {
	pa := AgentsToProto([]fleet.Agent{{
		Name: "nm", Command: "cmd", SystemPrompt: "sp", Backend: fleet.BackendCoder,
	}})[0]
	if pa.GetName() != "nm" || pa.GetCommand() != "cmd" || pa.GetSystemPrompt() != "sp" ||
		pa.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODER {
		t.Fatalf("agent wire placement wrong: %+v", pa)
	}
}

func TestInstanceWirePlacement(t *testing.T) {
	created := time.Unix(1720000000, 0).UTC()
	pi := InstanceToProto(&fleet.Instance{
		Name: "nm", DisplayName: "dn", ContainerID: "ci", Config: "cf",
		WorkspaceDir: "wd", CreatedAt: created, Status: fleet.StatusRunning,
		Error: "er", Backend: fleet.BackendCodespaces, Tag: "tg", Color: "co",
		Branch: "br", Automated: true,
	})
	if pi.GetName() != "nm" || pi.GetDisplayName() != "dn" || pi.GetContainerId() != "ci" ||
		pi.GetConfig() != "cf" || pi.GetWorkspaceDir() != "wd" ||
		pi.GetStatus() != fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING ||
		pi.GetError() != "er" || pi.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES ||
		pi.GetTag() != "tg" || pi.GetColor() != "co" || pi.GetBranch() != "br" ||
		!pi.GetAutomated() || !pi.GetCreatedAt().AsTime().Equal(created) {
		t.Fatalf("instance wire placement wrong: %+v", pi)
	}
}

func TestFleetSettingsWirePlacement(t *testing.T) {
	prefer := true
	ps := FleetSettingsToProto(fleet.FleetSettings{
		CustomMounts:       []string{"/m1", "/m2"},
		HomeDir:            "hd",
		CoderTemplate:      "ct",
		CoderPreset:        "cp",
		CoderWorkspaceName: "cw",
		PreferFleetLaunch:  &prefer,
		LayoutPresets:      []fleet.LayoutPreset{{Name: "lp", Layout: "ly", PaneCommands: []string{"pc"}}},
		Agents:             []fleet.Agent{{Name: "ag"}},
		Triggers:           []fleet.Trigger{{Name: "tr"}},
		CoderParameters: []fleet.CoderParameter{{
			Name: "pn", Value: "pv", DefaultValue: "pd", DisplayName: "pl", Description: "pe", Type: "py",
		}},
	})
	if ps.GetHomeDir() != "hd" || ps.GetCoderTemplate() != "ct" || ps.GetCoderPreset() != "cp" ||
		ps.GetCoderWorkspaceName() != "cw" || !ps.GetPreferFleetLaunch() ||
		len(ps.GetCustomMounts()) != 2 || ps.GetCustomMounts()[0] != "/m1" {
		t.Fatalf("settings scalar wire placement wrong: %+v", ps)
	}
	if lp := ps.GetLayoutPresets()[0]; lp.GetName() != "lp" || lp.GetLayout() != "ly" || lp.GetPaneCommands()[0] != "pc" {
		t.Fatalf("layout preset wire placement wrong: %+v", lp)
	}
	if ps.GetAgents()[0].GetName() != "ag" || ps.GetTriggers()[0].GetName() != "tr" {
		t.Fatalf("agent/trigger list wire placement wrong: %+v", ps)
	}
	if cp := ps.GetCoderParameters()[0]; cp.GetName() != "pn" || cp.GetValue() != "pv" ||
		cp.GetDefaultValue() != "pd" || cp.GetDisplayName() != "pl" ||
		cp.GetDescription() != "pe" || cp.GetType() != "py" {
		t.Fatalf("coder parameter wire placement wrong: %+v", cp)
	}

	// The mount/cache toggles are all bools, so value-distinctness can't tell
	// them apart: set exactly one per iteration and assert ITS getter reads
	// true — a swapped placement reads its neighbor's false and fails.
	boolFields := []struct {
		name string
		set  func(*fleet.FleetSettings)
		get  func(*fleetgrpc.FleetSettings) bool
	}{
		{"ClaudeCodeMount", func(s *fleet.FleetSettings) { s.ClaudeCodeMount = true }, (*fleetgrpc.FleetSettings).GetClaudeCodeMount},
		{"CodexMount", func(s *fleet.FleetSettings) { s.CodexMount = true }, (*fleetgrpc.FleetSettings).GetCodexMount},
		{"GhMount", func(s *fleet.FleetSettings) { s.GhMount = true }, (*fleetgrpc.FleetSettings).GetGhMount},
		{"AuggieMount", func(s *fleet.FleetSettings) { s.AuggieMount = true }, (*fleetgrpc.FleetSettings).GetAuggieMount},
		{"BuildkitServer", func(s *fleet.FleetSettings) { s.BuildkitServer = true }, (*fleetgrpc.FleetSettings).GetBuildkitServer},
		{"DebCacheServer", func(s *fleet.FleetSettings) { s.DebCacheServer = true }, (*fleetgrpc.FleetSettings).GetDebCacheServer},
		{"ImageCacheServer", func(s *fleet.FleetSettings) { s.ImageCacheServer = true }, (*fleetgrpc.FleetSettings).GetImageCacheServer},
	}
	for _, bf := range boolFields {
		var s fleet.FleetSettings
		bf.set(&s)
		if !bf.get(FleetSettingsToProto(s)) {
			t.Errorf("bool %s landed in the wrong proto field", bf.name)
		}
	}
}

func TestConfigWirePlacement(t *testing.T) {
	vim := false
	multi := true
	pc := ConfigToProto(&state.Config{
		GeneralSettings:    state.GeneralSettings{TmuxVimKeys: &vim},
		AgentSettings:      state.AgentSettings{ToolSelection: state.AgentToolCodex},
		DotfilesSettings:   state.DotfilesSettings{AutoInstall: true, RepoURL: "ru", InstallScript: "is"},
		CodespacesSettings: state.CodespacesSettings{Machine: "ma", IdleTimeout: "it", DevcontainerPath: "dp"},
		BrowserSettings:    state.BrowserSettings{MultipleBrowsersPerFleet: &multi},
		RemoteMcpSettings:  state.RemoteMcpSettings{GatewayURL: "gu"},
		DefaultBackend:     string(fleet.BackendCoder),
	})
	if pc.GetGeneral().TmuxVimKeys == nil || pc.GetGeneral().GetTmuxVimKeys() ||
		pc.GetGeneral().ShowHelpText != nil {
		t.Fatalf("general tri-states wire placement wrong: %+v", pc.GetGeneral())
	}
	if pc.GetAgent().GetToolSelection() != "codex" {
		t.Fatalf("agent tool wire placement wrong: %+v", pc.GetAgent())
	}
	if d := pc.GetDotfiles(); !d.GetAutoInstall() || d.GetRepo() != "ru" || d.GetInstallScript() != "is" {
		t.Fatalf("dotfiles wire placement wrong: %+v", d)
	}
	if cs := pc.GetCodespaces(); cs.GetMachine() != "ma" || cs.GetIdleTimeout() != "it" || cs.GetDevcontainerPath() != "dp" {
		t.Fatalf("codespaces wire placement wrong: %+v", cs)
	}
	if b := pc.GetBrowser(); !b.GetMultipleBrowsersPerFleet() || b.AutoSwitch != nil {
		t.Fatalf("browser tri-states wire placement wrong: %+v", b)
	}
	if pc.GetRemoteMcp().GetGatewayUrl() != "gu" {
		t.Fatalf("remote-mcp url wire placement wrong: %+v", pc.GetRemoteMcp())
	}
	if pc.GetDefaultBackend() != fleetgrpc.BackendType_BACKEND_TYPE_CODER {
		t.Fatalf("default backend wire placement wrong: %v", pc.GetDefaultBackend())
	}

	// RemoteMcp's three bools, one per iteration (same rationale as the
	// FleetSettings toggles).
	rmBools := []struct {
		name string
		set  func(*state.RemoteMcpSettings)
		get  func(*fleetgrpc.RemoteMcpSettings) bool
	}{
		{"Enabled", func(r *state.RemoteMcpSettings) { r.Enabled = true }, (*fleetgrpc.RemoteMcpSettings).GetEnabled},
		{"FleetEnabled", func(r *state.RemoteMcpSettings) { r.FleetEnabled = true }, (*fleetgrpc.RemoteMcpSettings).GetFleetEnabled},
		{"WebhookEnabled", func(r *state.RemoteMcpSettings) { r.WebhookEnabled = true }, (*fleetgrpc.RemoteMcpSettings).GetWebhookEnabled},
	}
	for _, rb := range rmBools {
		var c state.Config
		rb.set(&c.RemoteMcpSettings)
		if !rb.get(ConfigToProto(&c).GetRemoteMcp()) {
			t.Errorf("remote-mcp bool %s landed in the wrong proto field", rb.name)
		}
	}
}

func TestGroupLayoutWirePlacement(t *testing.T) {
	pgl := GroupLayoutToProto(state.GroupLayout{
		GroupID: "gi", InstanceName: "in", Sessions: []string{"s1"}, Layout: "ly", PaneCount: 7,
	})
	if pgl.GetGroupId() != "gi" || pgl.GetInstanceName() != "in" ||
		pgl.GetSessions()[0] != "s1" || pgl.GetLayout() != "ly" || pgl.GetPaneCount() != 7 {
		t.Fatalf("group layout wire placement wrong: %+v", pgl)
	}
}

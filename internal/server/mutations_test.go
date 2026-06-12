package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isolateFleetDir points $HOME at a temp dir so state.Load/Save and config
// read/write hit a throwaway ~/.fleet for the duration of the test.
func isolateFleetDir(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestCreateFleetPersistsAndIsIdempotent(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	reply, err := svc.CreateFleet(ctx, &fleetgrpc.CreateFleetRequest{Name: "alpha", Remote: "git@example.com:a.git"})
	if err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if f := reply.GetState().GetFleets()["alpha"]; f == nil || f.GetRemote() != "git@example.com:a.git" {
		t.Fatalf("reply missing fleet alpha: %v", reply.GetState().GetFleets())
	}

	// Persisted to disk.
	st, err := state.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := st.Fleets["alpha"]; !ok {
		t.Fatalf("fleet alpha not persisted")
	}

	// Idempotent: a second create keeps the original remote (GetOrCreateFleet).
	reply, err = svc.CreateFleet(ctx, &fleetgrpc.CreateFleetRequest{Name: "alpha", Remote: "ignored"})
	if err != nil {
		t.Fatalf("CreateFleet (2nd): %v", err)
	}
	if got := reply.GetState().GetFleets()["alpha"].GetRemote(); got != "git@example.com:a.git" {
		t.Fatalf("idempotent create changed remote to %q", got)
	}

	// Empty name rejected.
	if _, err := svc.CreateFleet(ctx, &fleetgrpc.CreateFleetRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty name, got %v", err)
	}
}

func TestDestroyFleetRejectsNonEmptyAndIsIdempotent(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	seed := &state.State{Fleets: map[string]*fleet.Fleet{
		"empty": {Name: "empty"},
		"full":  {Name: "full", Instances: []*fleet.Instance{{Name: "i1"}}},
	}}
	if err := state.Save(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Non-empty fleet rejected.
	if _, err := svc.DestroyFleet(ctx, &fleetgrpc.DestroyFleetRequest{Name: "full"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition destroying non-empty fleet, got %v", err)
	}

	// Empty fleet removed.
	reply, err := svc.DestroyFleet(ctx, &fleetgrpc.DestroyFleetRequest{Name: "empty"})
	if err != nil {
		t.Fatalf("DestroyFleet empty: %v", err)
	}
	if _, ok := reply.GetState().GetFleets()["empty"]; ok {
		t.Fatalf("empty fleet still present after destroy")
	}

	// Missing fleet is a no-op success.
	if _, err := svc.DestroyFleet(ctx, &fleetgrpc.DestroyFleetRequest{Name: "ghost"}); err != nil {
		t.Fatalf("DestroyFleet missing should be no-op, got %v", err)
	}
}

func TestSetFleetSettingsPreservesPresence(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prefer := true
	reply, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet: "alpha",
		Settings: &fleetgrpc.FleetSettings{
			ClaudeCodeMount:   true,
			GhMount:           true,
			AuggieMount:       true,
			BuildkitServer:    true,
			HomeDir:           ptr("/home/vscode"),
			PreferFleetLaunch: &prefer,
		},
	})
	if err != nil {
		t.Fatalf("SetFleetSettings: %v", err)
	}
	got := reply.GetState().GetFleets()["alpha"].GetSettings()
	if !got.GetClaudeCodeMount() || got.GetCodexMount() || !got.GetGhMount() || !got.GetAuggieMount() || got.GetHomeDir() != "/home/vscode" {
		t.Fatalf("settings mismatch: %v", got)
	}
	if !got.GetBuildkitServer() {
		t.Fatalf("BuildkitServer lost in reply: %v", got)
	}

	// Verify the persisted tri-state survives (PreferFleetLaunch set to true).
	st, _ := state.Load()
	s := st.Fleets["alpha"].Settings
	if s.PreferFleetLaunch == nil || !*s.PreferFleetLaunch {
		t.Fatalf("PreferFleetLaunch tri-state lost: %+v", s)
	}
	if !s.BuildkitServer {
		t.Fatalf("BuildkitServer not persisted: %+v", s)
	}

	// Unknown fleet -> NotFound.
	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{Fleet: "ghost", Settings: &fleetgrpc.FleetSettings{}}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TestSetFleetSettingsCustomMounts verifies the server normalizes (cleans +
// de-dups) valid custom mounts and rejects traversal/relative ones with
// InvalidArgument — the authoritative trust boundary.
func TestSetFleetSettingsCustomMounts(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Valid list with a duplicate + trailing slash gets canonicalized.
	reply, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet: "alpha",
		Settings: &fleetgrpc.FleetSettings{
			CustomMounts: []string{"/opt/data/", "/opt/data", "/var/cache"},
		},
	})
	if err != nil {
		t.Fatalf("SetFleetSettings: %v", err)
	}
	got := reply.GetState().GetFleets()["alpha"].GetSettings().GetCustomMounts()
	want := []string{"/opt/data", "/var/cache"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("custom mounts = %v, want %v", got, want)
	}

	// A traversal entry is rejected before persisting.
	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet:    "alpha",
		Settings: &fleetgrpc.FleetSettings{CustomMounts: []string{"/opt", "../../etc"}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for traversal mount, got %v", err)
	}

	// A relative entry is rejected too.
	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet:    "alpha",
		Settings: &fleetgrpc.FleetSettings{CustomMounts: []string{"relative/path"}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for relative mount, got %v", err)
	}

	// The rejected updates left the previously-saved valid list intact.
	st, _ := state.Load()
	if mounts := st.Fleets["alpha"].Settings.CustomMounts; len(mounts) != 2 {
		t.Fatalf("persisted custom mounts = %v, want the 2 valid entries", mounts)
	}
}

// TestSetFleetSettingsLayoutPresets verifies layout presets round-trip through
// the RPC + state.json (name, layout string, per-pane commands — including the
// empty "plain shell" command) and that the server rejects invalid lists with
// InvalidArgument, like custom mounts.
func TestSetFleetSettingsLayoutPresets(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	layout := "a0c2,208x58,0,0{104x58,0,0,0,103x58,105,0[103x29,105,0,1,103x28,105,30,2]}"
	reply, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet: "alpha",
		Settings: &fleetgrpc.FleetSettings{
			LayoutPresets: []*fleetgrpc.LayoutPreset{
				{Name: " app-run ", Layout: layout, PaneCommands: []string{"npm run dev", ""}},
			},
		},
	})
	if err != nil {
		t.Fatalf("SetFleetSettings: %v", err)
	}
	got := reply.GetState().GetFleets()["alpha"].GetSettings().GetLayoutPresets()
	if len(got) != 1 || got[0].GetName() != "app-run" || got[0].GetLayout() != layout {
		t.Fatalf("presets in reply = %v", got)
	}
	if cmds := got[0].GetPaneCommands(); len(cmds) != 2 || cmds[0] != "npm run dev" || cmds[1] != "" {
		t.Fatalf("pane commands = %v", got[0].GetPaneCommands())
	}

	// Persisted through the legacy state.json mapper.
	st, _ := state.Load()
	presets := st.Fleets["alpha"].Settings.LayoutPresets
	if len(presets) != 1 || presets[0].Name != "app-run" || presets[0].Layout != layout || len(presets[0].PaneCommands) != 2 {
		t.Fatalf("persisted presets = %+v", presets)
	}

	// Duplicate names are rejected before persisting.
	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet: "alpha",
		Settings: &fleetgrpc.FleetSettings{LayoutPresets: []*fleetgrpc.LayoutPreset{
			{Name: "x", PaneCommands: []string{""}},
			{Name: "x", PaneCommands: []string{""}},
		}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for duplicate preset names, got %v", err)
	}

	// A pane-less preset is rejected.
	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
		Fleet:    "alpha",
		Settings: &fleetgrpc.FleetSettings{LayoutPresets: []*fleetgrpc.LayoutPreset{{Name: "x"}}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for pane-less preset, got %v", err)
	}

	// The rejected updates left the valid preset intact.
	st, _ = state.Load()
	if presets := st.Fleets["alpha"].Settings.LayoutPresets; len(presets) != 1 || presets[0].Name != "app-run" {
		t.Fatalf("persisted presets after rejects = %+v", presets)
	}
}

func TestSetInstanceMetadataUpdatesOnlyProvidedFields(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", DisplayName: "Orig", Color: "red", Tag: "keep"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Update display_name + color, leave tag untouched (nil).
	reply, err := svc.SetInstanceMetadata(ctx, &fleetgrpc.SetInstanceMetadataRequest{
		Fleet: "alpha", Instance: "i1",
		DisplayName: ptr("Renamed"), Color: ptr("blue"),
	})
	if err != nil {
		t.Fatalf("SetInstanceMetadata: %v", err)
	}
	inst := reply.GetState().GetFleets()["alpha"].GetInstances()[0]
	if inst.GetDisplayName() != "Renamed" || inst.GetColor() != "blue" || inst.GetTag() != "keep" {
		t.Fatalf("partial update wrong: name=%q color=%q tag=%q", inst.GetDisplayName(), inst.GetColor(), inst.GetTag())
	}

	// Clear the tag with an explicit empty string (presence bit set).
	reply, err = svc.SetInstanceMetadata(ctx, &fleetgrpc.SetInstanceMetadataRequest{
		Fleet: "alpha", Instance: "i1", Tag: ptr(""),
	})
	if err != nil {
		t.Fatalf("SetInstanceMetadata clear tag: %v", err)
	}
	st, _ := state.Load()
	if got := st.Fleets["alpha"].Instances[0].Tag; got != "" {
		t.Fatalf("tag not cleared: %q", got)
	}

	// Unknown instance -> NotFound.
	if _, err := svc.SetInstanceMetadata(ctx, &fleetgrpc.SetInstanceMetadataRequest{Fleet: "alpha", Instance: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestSetInstanceMetadataRejectsDisplayNameWithSpaces(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", DisplayName: "orig"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := svc.SetInstanceMetadata(ctx, &fleetgrpc.SetInstanceMetadataRequest{
		Fleet: "alpha", Instance: "i1", DisplayName: ptr("new name"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	st, _ := state.Load()
	if got := st.Fleets["alpha"].Instances[0].DisplayName; got != "orig" {
		t.Fatalf("DisplayName = %q, want unchanged %q", got, "orig")
	}

	// An explicit empty string clears the override (falls back to Name) and
	// must stay allowed.
	if _, err := svc.SetInstanceMetadata(ctx, &fleetgrpc.SetInstanceMetadataRequest{
		Fleet: "alpha", Instance: "i1", DisplayName: ptr(""),
	}); err != nil {
		t.Fatalf("clear display name: %v", err)
	}
}

func TestGroupLayoutSetAndDelete(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	_, err := svc.SetGroupLayout(ctx, &fleetgrpc.SetGroupLayoutRequest{Layout: &fleetgrpc.GroupLayout{
		GroupId: "g1", InstanceName: "i1", Sessions: []string{"s1", "s2"}, Layout: "abc", PaneCount: 2,
	}})
	if err != nil {
		t.Fatalf("SetGroupLayout: %v", err)
	}
	st, _ := state.Load()
	gl, ok := st.GroupLayouts["i1/g1"]
	if !ok || gl.Layout != "abc" || gl.PaneCount != 2 || len(gl.Sessions) != 2 {
		t.Fatalf("layout not persisted under composite key: %+v", st.GroupLayouts)
	}

	if _, err := svc.DeleteGroupLayout(ctx, &fleetgrpc.DeleteGroupLayoutRequest{InstanceName: "i1", GroupId: "g1"}); err != nil {
		t.Fatalf("DeleteGroupLayout: %v", err)
	}
	st, _ = state.Load()
	if _, ok := st.GroupLayouts["i1/g1"]; ok {
		t.Fatalf("layout not deleted")
	}

	// Missing layout in request -> InvalidArgument.
	if _, err := svc.SetGroupLayout(ctx, &fleetgrpc.SetGroupLayoutRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestSetLastSeenVersion(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	reply, err := svc.SetLastSeenVersion(context.Background(), &fleetgrpc.SetLastSeenVersionRequest{Version: "v1.2.3"})
	if err != nil {
		t.Fatalf("SetLastSeenVersion: %v", err)
	}
	if reply.GetState().GetLastSeenVersion() != "v1.2.3" {
		t.Fatalf("reply version mismatch: %q", reply.GetState().GetLastSeenVersion())
	}
	st, _ := state.Load()
	if st.LastSeenVersion != "v1.2.3" {
		t.Fatalf("not persisted: %q", st.LastSeenVersion)
	}
}

// proto returns a pointer to v — a tiny helper for the optional scalar fields.
func ptr[T any](v T) *T { return &v }

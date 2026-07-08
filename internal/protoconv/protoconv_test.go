package protoconv

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The reflection round-trips below are the package's main guard: fill EVERY
// field of a domain struct with a distinct non-zero value, run it through
// to-proto -> from-proto, and require deep equality. Adding a field to a
// domain struct without teaching its converter about it fails these tests
// automatically — the "new field must be added in 6 places" trap this package
// replaced can no longer drop fields silently.

// fillValue populates v with deterministic non-zero data. Named enum types get
// a valid member (their converters map via switch, so arbitrary strings would
// not survive); everything else gets a value derived from path so mismatches
// name the exact field.
func fillValue(t *testing.T, v reflect.Value, path string) {
	t.Helper()

	// Enum-ish named types whose converters only pass valid members through.
	switch v.Type() {
	case reflect.TypeFor[fleet.InstanceStatus]():
		v.Set(reflect.ValueOf(fleet.StatusRebuilding))
		return
	case reflect.TypeFor[fleet.BackendType]():
		v.Set(reflect.ValueOf(fleet.BackendCoder))
		return
	case reflect.TypeFor[time.Time]():
		// UTC wall-clock time: timestamppb round-trips to a UTC time.Time, so a
		// zoned or monotonic-clock-carrying value would fail DeepEqual for
		// reasons that have nothing to do with the converters.
		v.Set(reflect.ValueOf(time.Unix(1720000000, 123456789).UTC()))
		return
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString("v:" + path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		// Leave a *bool pointing at false: presence-vs-value is exactly the
		// tri-state distinction the converters must preserve.
		if p.Elem().Kind() != reflect.Bool {
			fillValue(t, p.Elem(), path)
		}
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range 2 {
			fillValue(t, s.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillValue(t, key, path+".key")
		val := reflect.New(v.Type().Elem()).Elem()
		fillValue(t, val, path+".val")
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillValue(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	default:
		t.Fatalf("fillValue: unhandled kind %s at %s — extend the filler", v.Kind(), path)
	}
}

func filled[T any](t *testing.T) T {
	t.Helper()
	var out T
	fillValue(t, reflect.ValueOf(&out).Elem(), reflect.TypeOf(out).Name())
	return out
}

func requireEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch (a converter is dropping a field):\n got: %+v\nwant: %+v", got, want)
	}
}

func TestTriggerRoundTripAllFields(t *testing.T) {
	in := []fleet.Trigger{filled[fleet.Trigger](t)}
	requireEqual(t, TriggersFromProto(TriggersToProto(in)), in)
}

func TestAgentRoundTripAllFields(t *testing.T) {
	in := []fleet.Agent{filled[fleet.Agent](t)}
	requireEqual(t, AgentsFromProto(AgentsToProto(in)), in)
}

func TestLayoutPresetRoundTripAllFields(t *testing.T) {
	in := []fleet.LayoutPreset{filled[fleet.LayoutPreset](t)}
	requireEqual(t, LayoutPresetsFromProto(LayoutPresetsToProto(in)), in)
}

func TestCoderParameterRoundTripAllFields(t *testing.T) {
	in := []fleet.CoderParameter{filled[fleet.CoderParameter](t)}
	requireEqual(t, CoderParametersFromProto(CoderParametersToProto(in)), in)
}

func TestFleetSettingsRoundTripAllFields(t *testing.T) {
	in := filled[fleet.FleetSettings](t)
	requireEqual(t, FleetSettingsFromProto(FleetSettingsToProto(in)), in)
}

func TestInstanceRoundTripAllFields(t *testing.T) {
	in := filled[fleet.Instance](t)
	requireEqual(t, InstanceFromProto(InstanceToProto(&in)), &in)
}

func TestGroupLayoutRoundTripAllFields(t *testing.T) {
	in := filled[state.GroupLayout](t)
	requireEqual(t, GroupLayoutFromProto(GroupLayoutToProto(in)), in)
}

func TestStateRoundTripAllFields(t *testing.T) {
	in := filled[state.State](t)
	requireEqual(t, StateFromProto(StateToProto(&in)), &in)
}

func TestConfigRoundTripAllFields(t *testing.T) {
	in := filled[state.Config](t)
	// DefaultBackend is a plain string carrying enum semantics (the one field
	// the generic filler can't know about): only valid backend names survive.
	in.DefaultBackend = string(fleet.BackendCodespaces)
	requireEqual(t, ConfigFromProto(ConfigToProto(&in), &state.Config{}), &in)
}

// --- Intent-specific cases the fuzz fill can't express -----------------------

// TestEnumsExhaustive proves every enum member survives both directions; a new
// domain status/backend added without a proto mapping falls to UNSPECIFIED and
// fails here.
func TestEnumsExhaustive(t *testing.T) {
	for _, s := range []fleet.InstanceStatus{
		fleet.StatusCreating, fleet.StatusCloning, fleet.StatusRunning,
		fleet.StatusStopped, fleet.StatusFailed, fleet.StatusStopping,
		fleet.StatusStarting, fleet.StatusDeleting, fleet.StatusRebuilding,
	} {
		if got := StatusFromProto(StatusToProto(s)); got != s {
			t.Errorf("status %q round-tripped to %q", s, got)
		}
	}
	if StatusToProto("") != fleetgrpc.InstanceStatus_INSTANCE_STATUS_UNSPECIFIED || StatusFromProto(fleetgrpc.InstanceStatus_INSTANCE_STATUS_UNSPECIFIED) != "" {
		t.Error("empty status should map to UNSPECIFIED and back")
	}

	for _, b := range []fleet.BackendType{fleet.BackendDevcontainer, fleet.BackendCoder, fleet.BackendCodespaces} {
		if got := BackendFromProto(BackendToProto(b)); got != b {
			t.Errorf("backend %q round-tripped to %q", b, got)
		}
	}
	if BackendToProto("") != fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED || BackendFromProto(fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED) != "" {
		t.Error("empty backend should map to UNSPECIFIED and back")
	}
}

// TestEmptyAndNilHandling locks the wire conventions: empty repeated fields map
// to nil (matching the `,omitempty` legacy JSON), nil protos map to usable zero
// values, and StateFromProto always returns non-nil maps.
func TestEmptyAndNilHandling(t *testing.T) {
	if TriggersToProto(nil) != nil || TriggersFromProto(nil) != nil {
		t.Error("empty trigger lists should map to nil both ways")
	}
	if AgentsToProto(nil) != nil || AgentsFromProto(nil) != nil {
		t.Error("empty agent lists should map to nil both ways")
	}
	if LayoutPresetsToProto(nil) != nil || LayoutPresetsFromProto(nil) != nil {
		t.Error("empty preset lists should map to nil both ways")
	}
	if CoderParametersToProto(nil) != nil || CoderParametersFromProto(nil) != nil {
		t.Error("empty coder parameter lists should map to nil both ways")
	}

	if got := FleetSettingsFromProto(nil); !reflect.DeepEqual(got, fleet.FleetSettings{}) {
		t.Errorf("nil settings proto should map to the zero value, got %+v", got)
	}

	st := StateFromProto(nil)
	if st.Fleets == nil || st.GroupLayouts == nil {
		t.Error("StateFromProto must return non-nil maps even for a nil proto")
	}

	f := FleetFromProto(&fleetgrpc.Fleet{Name: "x"})
	if f.Instances == nil {
		t.Error("FleetFromProto must return a non-nil Instances slice")
	}
}

// TestFleetSettingsTriState checks the PreferFleetLaunch nil-vs-set distinction
// survives: unset stays nil on the wire, explicit false stays present-and-false.
func TestFleetSettingsTriState(t *testing.T) {
	if got := FleetSettingsToProto(fleet.FleetSettings{}); got.PreferFleetLaunch != nil {
		t.Fatalf("unset PreferFleetLaunch should be nil on the wire, got %v", *got.PreferFleetLaunch)
	}
	f := false
	ps := FleetSettingsToProto(fleet.FleetSettings{PreferFleetLaunch: &f})
	if ps.PreferFleetLaunch == nil || ps.GetPreferFleetLaunch() {
		t.Fatalf("explicit-false PreferFleetLaunch lost: %v", ps.PreferFleetLaunch)
	}
	back := FleetSettingsFromProto(ps)
	if back.PreferFleetLaunch == nil || *back.PreferFleetLaunch {
		t.Fatalf("explicit-false PreferFleetLaunch lost on the way back: %v", back.PreferFleetLaunch)
	}
}

// TestInstancePresenceRules locks the wire presence conventions inherited from
// the legacy JSON tags: non-omitempty fields are always sent (even empty), and
// omitempty scalars travel only when set.
func TestInstancePresenceRules(t *testing.T) {
	pi := InstanceToProto(&fleet.Instance{Name: "x"})
	if pi.ContainerId == nil || pi.Config == nil || pi.WorkspaceDir == nil {
		t.Error("container_id/config/workspace_dir must always be present")
	}
	if pi.DisplayName != nil || pi.Error != nil || pi.Tag != nil || pi.Color != nil || pi.Branch != nil {
		t.Error("unset omitempty scalars must be absent from the wire")
	}
	if pi.CreatedAt != nil {
		t.Error("a zero CreatedAt must be absent from the wire")
	}
	if pi.GetAutomated() {
		t.Error("a non-automated instance should map to automated=false")
	}
	if !InstanceToProto(&fleet.Instance{Name: "x", Automated: true}).GetAutomated() {
		t.Error("InstanceToProto should carry Automated=true")
	}
}

// TestConfigFromProtoBases locks the one deliberate divergence between the two
// consolidated callers: the server write path (zero base) must not invent
// defaults, while the TUI read path (DefaultConfig base) must keep them for
// fields the proto leaves unset.
func TestConfigFromProtoBases(t *testing.T) {
	empty := &fleetgrpc.Config{Agent: &fleetgrpc.AgentSettings{}}

	if got := ConfigFromProto(empty, &state.Config{}); got.AgentSettings.ToolSelection != "" {
		t.Errorf("zero base: empty tool selection should stay empty (SaveConfig normalizes), got %q", got.AgentSettings.ToolSelection)
	}
	if got := ConfigFromProto(empty, state.DefaultConfig()); got.AgentSettings.ToolSelection != state.AgentToolClaude {
		t.Errorf("default base: unset tool selection should keep the default, got %q", got.AgentSettings.ToolSelection)
	}
	if got := ConfigFromProto(nil, nil); got == nil {
		t.Error("nil proto + nil base must still return a usable config")
	}

	// A set tool selection wins over either base.
	set := &fleetgrpc.Config{Agent: &fleetgrpc.AgentSettings{ToolSelection: "codex"}}
	if got := ConfigFromProto(set, state.DefaultConfig()); got.AgentSettings.ToolSelection != state.AgentToolCodex {
		t.Errorf("set tool selection should override the base default, got %q", got.AgentSettings.ToolSelection)
	}
}

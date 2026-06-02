package fleetgrpc

import "testing"

func TestInstanceStatusDisplay(t *testing.T) {
	cases := map[InstanceStatus]string{
		InstanceStatus_INSTANCE_STATUS_UNSPECIFIED: "unknown",
		InstanceStatus_INSTANCE_STATUS_CREATING:    "creating",
		InstanceStatus_INSTANCE_STATUS_CLONING:     "cloning",
		InstanceStatus_INSTANCE_STATUS_RUNNING:     "running",
		InstanceStatus_INSTANCE_STATUS_STOPPED:     "stopped",
		InstanceStatus_INSTANCE_STATUS_FAILED:      "failed",
		InstanceStatus_INSTANCE_STATUS_STOPPING:    "stopping",
		InstanceStatus_INSTANCE_STATUS_STARTING:    "starting",
		InstanceStatus_INSTANCE_STATUS_DELETING:    "deleting",
	}
	for status, want := range cases {
		if got := status.Display(); got != want {
			t.Errorf("InstanceStatus(%v).Display() = %q, want %q", status, got, want)
		}
	}
}

func TestBackendTypeDisplay(t *testing.T) {
	cases := map[BackendType]string{
		BackendType_BACKEND_TYPE_UNSPECIFIED: "",
		BackendType_BACKEND_TYPE_DEVCONTAINER: "devcontainer",
		BackendType_BACKEND_TYPE_CODER:        "coder",
		BackendType_BACKEND_TYPE_CODESPACES:   "codespaces",
	}
	for backend, want := range cases {
		if got := backend.Display(); got != want {
			t.Errorf("BackendType(%v).Display() = %q, want %q", backend, got, want)
		}
	}
}

func TestInstanceDisplayNameOrName(t *testing.T) {
	withDisplay := &Instance{Name: "agent-1", DisplayName: proto("pretty")}
	if got := withDisplay.DisplayNameOrName(); got != "pretty" {
		t.Errorf("DisplayNameOrName() with display set = %q, want %q", got, "pretty")
	}

	legacy := &Instance{Name: "agent-1"} // no DisplayName (nil optional)
	if got := legacy.DisplayNameOrName(); got != "agent-1" {
		t.Errorf("DisplayNameOrName() legacy fallback = %q, want %q", got, "agent-1")
	}
}

func TestFleetFindInstance(t *testing.T) {
	f := &Fleet{Name: "alpha", Instances: []*Instance{{Name: "a"}, {Name: "b"}}}
	if inst, ok := f.FindInstance("b"); !ok || inst.GetName() != "b" {
		t.Errorf("FindInstance(b) = %v, %v; want instance b, true", inst, ok)
	}
	if inst, ok := f.FindInstance("missing"); ok || inst != nil {
		t.Errorf("FindInstance(missing) = %v, %v; want nil, false", inst, ok)
	}
}

func proto(s string) *string { return &s }

package fleet

import "testing"

func TestDeployTargetsListEveryBackendTypeInDialogOrder(t *testing.T) {
	got := DeployTargets()
	want := []BackendType{BackendDevcontainer, BackendCoder, BackendCodespaces}
	if len(got) != len(want) {
		t.Fatalf("DeployTargets() = %v, want backends %v", got, want)
	}
	for i, target := range got {
		if target.Backend != want[i] {
			t.Errorf("DeployTargets()[%d] = %v, want %v", i, target.Backend, want[i])
		}
	}

	// Every type the parser accepts is a target; add a new backend type here.
	for _, value := range []string{"devcontainer", "coder", "codespaces"} {
		backendType, err := ParseBackendType(value)
		if err != nil {
			t.Fatalf("ParseBackendType(%q) error = %v", value, err)
		}
		found := false
		for _, target := range got {
			found = found || target.Backend == backendType
		}
		if !found {
			t.Errorf("BackendType %q has no deploy target", backendType)
		}
	}

	// The returned slice is the caller's own.
	got[0].Backend = "mutated"
	if DeployTargets()[0].Backend != BackendDevcontainer {
		t.Fatal("DeployTargets() shares its backing array with the caller")
	}
}

func TestDeployTargetTool(t *testing.T) {
	tests := map[BackendType]string{
		BackendDevcontainer: "devcontainer",
		BackendCoder:        "coder",
		BackendCodespaces:   "gh",
		"":                  "",
	}
	for backendType, want := range tests {
		if got := (DeployTarget{Backend: backendType}).Tool(); got != want {
			t.Errorf("DeployTarget{%q}.Tool() = %q, want %q", backendType, got, want)
		}
	}
	for _, target := range DeployTargets() {
		if target.Tool() == "" {
			t.Errorf("deploy target %v has no tool row", target)
		}
	}
}

// CopiesTemplates must agree with the create-time rule, so the dialog never
// offers a target the server then rejects for a template fleet.
func TestDeployTargetCopiesTemplatesMatchesValidateTemplateCreate(t *testing.T) {
	for _, target := range DeployTargets() {
		err := ValidateTemplateCreate("file:///tmp/template", "", target.Backend)
		if got := err == nil; got != target.CopiesTemplates() {
			t.Errorf("%v: CopiesTemplates() = %v but ValidateTemplateCreate error = %v", target, target.CopiesTemplates(), err)
		}
	}
}

func TestDeployTargetZeroAndString(t *testing.T) {
	if !(DeployTarget{}).IsZero() {
		t.Fatal("zero DeployTarget should be IsZero")
	}
	target := DeployTarget{Backend: BackendCoder}
	if target.IsZero() {
		t.Fatal("coder target should not be IsZero")
	}
	if target.String() != "coder" {
		t.Fatalf("String() = %q, want coder", target.String())
	}
}

func TestInstanceDeployTarget(t *testing.T) {
	tests := []struct {
		backend BackendType
		want    BackendType
	}{
		{BackendCodespaces, BackendCodespaces},
		// Records from before the Backend field ran on devcontainer.
		{"", BackendDevcontainer},
		// A type this build does not know is kept, so its capabilities defer to
		// the server.
		{"some-future-backend", "some-future-backend"},
	}
	for _, tt := range tests {
		inst := &Instance{Backend: tt.backend}
		got := inst.DeployTarget()
		if got.Backend != tt.want {
			t.Errorf("Instance{Backend: %q}.DeployTarget() = %v, want %v", tt.backend, got, tt.want)
		}
		if got.Capabilities() != tt.want.Capabilities() {
			t.Errorf("%q: target capabilities %+v differ from the backend type's %+v", tt.backend, got.Capabilities(), tt.want.Capabilities())
		}
	}
}

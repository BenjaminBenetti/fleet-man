package fleet

import "testing"

func TestParseBackendType(t *testing.T) {
	tests := []struct {
		value   string
		want    BackendType
		wantErr bool
	}{
		{value: "devcontainer", want: BackendDevcontainer},
		{value: "coder", want: BackendCoder},
		{value: "codespaces", want: BackendCodespaces},
		{value: "kubernetes", want: BackendKubernetes},
		{value: "bogus", wantErr: true},
		{value: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := ParseBackendType(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseBackendType() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBackendType() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseBackendType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackendTypesListEveryKnownType(t *testing.T) {
	want := []BackendType{BackendDevcontainer, BackendCoder, BackendCodespaces, BackendKubernetes}
	got := BackendTypes()
	if len(got) != len(want) {
		t.Fatalf("BackendTypes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BackendTypes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Every type a new instance can name is a known type.
	for _, value := range []string{"devcontainer", "coder", "codespaces", "kubernetes"} {
		backendType, err := ParseBackendType(value)
		if err != nil {
			t.Fatalf("ParseBackendType(%q) error = %v", value, err)
		}
		found := false
		for _, known := range got {
			found = found || known == backendType
		}
		if !found {
			t.Errorf("ParseBackendType accepts %q, which BackendTypes() does not list", backendType)
		}
	}

	// The returned slice is the caller's own.
	got[0] = "mutated"
	if BackendTypes()[0] != BackendDevcontainer {
		t.Fatal("BackendTypes() shares its backing array with the caller")
	}
}

func TestValidateBackendType(t *testing.T) {
	for _, bt := range BackendTypes() {
		if err := ValidateBackendType(bt); err != nil {
			t.Errorf("ValidateBackendType(%s) = %v", bt, err)
		}
	}
	if err := ValidateBackendType("bogus"); err == nil {
		t.Error("ValidateBackendType(bogus) = nil")
	}
}

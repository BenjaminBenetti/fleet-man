package fleet

import "testing"

func TestBackendTypeCapabilities(t *testing.T) {
	tests := []struct {
		backend BackendType
		want    BackendCapabilities
	}{
		{BackendDevcontainer, BackendCapabilities{Clone: true, Rebuild: true}},
		// "" is how records from before the Backend field read back.
		{"", BackendCapabilities{Clone: true, Rebuild: true}},
		{BackendCodespaces, BackendCapabilities{Rebuild: true}},
		{BackendCoder, BackendCapabilities{}},
		// A type this build does not know defers to the server.
		{"some-future-backend", BackendCapabilities{Clone: true, Rebuild: true}},
	}
	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			if got := tt.backend.Capabilities(); got != tt.want {
				t.Fatalf("%q.Capabilities() = %+v, want %+v", tt.backend, got, tt.want)
			}
		})
	}
}

// TestEveryBackendTypeHasCapabilities checks the table against the parser: each
// type it accepts has a row (a type without one reports every capability), and
// each row is a type it accepts. Add a new backend type to the list here.
func TestEveryBackendTypeHasCapabilities(t *testing.T) {
	for _, value := range []string{"devcontainer", "coder", "codespaces"} {
		backendType, err := ParseBackendType(value)
		if err != nil {
			t.Fatalf("ParseBackendType(%q) error = %v", value, err)
		}
		if _, ok := backendCapabilities[backendType]; !ok {
			t.Errorf("BackendType %q has no backendCapabilities row", backendType)
		}
	}
	for backendType := range backendCapabilities {
		if err := ValidateBackendType(backendType); err != nil {
			t.Errorf("backendCapabilities row for unknown type %q: %v", backendType, err)
		}
	}
}

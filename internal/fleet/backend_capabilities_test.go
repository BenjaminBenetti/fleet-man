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
		{BackendKubernetes, BackendCapabilities{Clone: true, Rebuild: true}},
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

// TestEveryBackendTypeHasCapabilities checks the table against the known
// types: each has a row (a type without one reports every capability), and
// each row is a known type. BackendTypes() lists the known types, and its own
// test pins that list, so a new backend type lands in both.
func TestEveryBackendTypeHasCapabilities(t *testing.T) {
	known := map[BackendType]bool{}
	for _, backendType := range BackendTypes() {
		known[backendType] = true
		if _, ok := backendCapabilities[backendType]; !ok {
			t.Errorf("BackendType %q has no backendCapabilities row", backendType)
		}
	}
	for backendType := range backendCapabilities {
		if !known[backendType] {
			t.Errorf("backendCapabilities row for unknown type %q", backendType)
		}
	}
}

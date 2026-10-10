package backend

import "testing"

func TestMountScopeString(t *testing.T) {
	tests := []struct {
		scope MountScope
		want  string
	}{
		{MountScopeHost, "host"},
		{MountScopeFleetShared, "fleet-shared"},
		{MountScope(200), "MountScope(200)"},
	}
	for _, tt := range tests {
		if got := tt.scope.String(); got != tt.want {
			t.Errorf("MountScope(%d).String() = %q, want %q", uint8(tt.scope), got, tt.want)
		}
	}
}

// TestMountZeroValueIsHostScope pins the compatibility rule every existing
// Mount literal relies on: a mount built without a scope is a plain host bind.
func TestMountZeroValueIsHostScope(t *testing.T) {
	var mount Mount
	if mount.Scope != MountScopeHost {
		t.Fatalf("zero Mount.Scope = %v, want %v", mount.Scope, MountScopeHost)
	}
}

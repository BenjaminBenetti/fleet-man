package fleet

import "testing"

func TestValidateInstanceName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "agent-1"},
		{name: "Agent_1"},
		{name: "auth.fix"},
		{name: "", wantErr: true},
		{name: "my agent", wantErr: true},
		{name: " agent-1", wantErr: true},
		{name: "agent-1 ", wantErr: true},
		{name: "agent\t1", wantErr: true},
		{name: "agent\n1", wantErr: true},
		// The fleet's own folders beside the checkouts.
		{name: "files", wantErr: true},
		{name: ".claude", wantErr: true},
		{name: ".mnt", wantErr: true},
		{name: ".vol", wantErr: true},
		{name: ".hostbind", wantErr: true},
		{name: ".", wantErr: true},
		{name: "..", wantErr: true},
		{name: "files/x", wantErr: true},
		{name: "a\\b", wantErr: true},
		{name: "files-2"},
		{name: "my.files"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInstanceName(tt.name)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateInstanceName(%q) = nil, want error", tt.name)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateInstanceName(%q) = %v, want nil", tt.name, err)
			}
		})
	}
}

// A display name is a label: the folder rules of a name do not apply to it,
// only the emptiness and spaces rule they share.
func TestValidateDisplayName(t *testing.T) {
	for _, ok := range []string{"agent-1", "files", ".claude", "feat/login", "a\\b"} {
		if err := ValidateDisplayName(ok); err != nil {
			t.Errorf("ValidateDisplayName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "my agent", "agent\t1"} {
		if ValidateDisplayName(bad) == nil {
			t.Errorf("ValidateDisplayName(%q) = nil, want error", bad)
		}
	}
}

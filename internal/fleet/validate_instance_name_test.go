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

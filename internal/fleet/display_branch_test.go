package fleet

import "testing"

func TestDisplayBranch(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := []struct {
		name      string
		live      *string
		last, req string
		want      string
	}{
		{"live read wins", str("feat/x"), "main", "dev", "feat/x"},
		{"live detached head stays blank", str(""), "main", "dev", ""},
		{"no live read falls back to last seen", nil, "feat/y", "dev", "feat/y"},
		{"never seen falls back to requested", nil, "", "dev", "dev"},
		{"nothing known", nil, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayBranch(tt.live, tt.last, tt.req); got != tt.want {
				t.Fatalf("DisplayBranch() = %q, want %q", got, tt.want)
			}
		})
	}
}

package tui

import "testing"

func TestParseTmuxSessionsFiltersToInstancePrefix(t *testing.T) {
	// Mixed list: fleet-managed sessions for "alpha", a fleet-managed
	// session for "beta", a manually-named host session, and the outer
	// fleet TUI's own tmux session. Only alpha sessions should survive.
	out := `alpha:1:0
alpha~deadbeef:2:1
alpha~feedface~01:1:0
beta~aaaa:1:0
host-foo:1:0
fleetman-itest:1:1
`
	got := parseTmuxSessions(out, "alpha")
	wantNames := []string{"alpha", "alpha~deadbeef", "alpha~feedface~01"}
	if len(got) != len(wantNames) {
		t.Fatalf("parseTmuxSessions returned %d sessions, want %d: %+v", len(got), len(wantNames), got)
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Errorf("session[%d].Name = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestParseTmuxSessionsExactMatchKeepsLegacyName(t *testing.T) {
	// A session named exactly the sanitized instance (legacy non-grouped
	// shell session) must be kept.
	got := parseTmuxSessions("agent-1:1:0\n", "agent-1")
	if len(got) != 1 || got[0].Name != "agent-1" {
		t.Fatalf("parseTmuxSessions = %+v, want [{Name: agent-1}]", got)
	}
}

func TestParseTmuxSessionsRejectsUnrelatedSessions(t *testing.T) {
	// Sessions that don't start with the prefix or equal the sanitized
	// name must be dropped — even if they share a substring.
	out := `alpha-other:1:0
not-alpha~xx:1:0
`
	got := parseTmuxSessions(out, "alpha")
	if len(got) != 0 {
		t.Fatalf("parseTmuxSessions = %+v, want empty", got)
	}
}

func TestParseTmuxSessionsHandlesEmptyOutput(t *testing.T) {
	got := parseTmuxSessions("", "alpha")
	if len(got) != 0 {
		t.Fatalf("parseTmuxSessions(\"\") = %+v, want empty", got)
	}
}

func TestSanitizedFromInstanceKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"itest-fleet/alpha", "alpha"},
		{"itest-fleet/my.instance", "my-instance"},
		{"alpha", "alpha"},
		{"foo/bar/baz", "baz"},
	}
	for _, tt := range tests {
		got := sanitizedFromInstanceKey(tt.key)
		if got != tt.want {
			t.Errorf("sanitizedFromInstanceKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

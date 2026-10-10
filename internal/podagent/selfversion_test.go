package podagent

import (
	"strings"
	"testing"
)

func TestStagedSHA(t *testing.T) {
	sha := strings.Repeat("ab12", 16)
	for name, want := range map[string]string{
		"fleet-" + sha:                  sha,
		"fleet-" + strings.ToUpper(sha): "",
		"fleet-" + sha[:63]:             "",
		"fleet":                         "",
		"fleet-pod-linux-amd64":         "",
		"other-" + sha:                  "",
		"fleet-" + sha[:62] + "zz":      "",
	} {
		if got := stagedSHA(name); got != want {
			t.Errorf("stagedSHA(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSelfVersion(t *testing.T) {
	v := SelfVersion()
	if len(v.SHA) != 64 || v.ProtoMin < 1 || v.ProtoMax < v.ProtoMin || v.Arch == "" {
		t.Fatalf("%+v", v)
	}
}

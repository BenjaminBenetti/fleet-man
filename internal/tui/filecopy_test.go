package tui

import (
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

func TestResolveLocalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"absolute used as-is", "/abs/target", "/abs/target"},
		{"bare tilde is home", "~", home},
		{"tilde-slash resolves against home", "~/builds/tool", filepath.Join(home, "builds/tool")},
		{"relative resolves against home", "builds/tool", filepath.Join(home, "builds/tool")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLocalPath(tc.path); got != tc.want {
				t.Errorf("resolveLocalPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestTUILocalPolicyResolveDest covers the human-machine dest policy: empty goes
// to the downloads folder (no Downloads dir in the temp home, so home itself),
// a directory keeps the source name, and a file path is used as-is.
func TestTUILocalPolicyResolveDest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	policy := tuiLocalPolicy{}

	cases := []struct {
		name, dest, src, want string
	}{
		{"empty means downloads", "", "tool", filepath.Join(home, "tool")},
		{"trailing slash keeps name", "~/builds/", "tool", filepath.Join(home, "builds", "tool")},
		{"file path used as-is", "~/out.bin", "tool", filepath.Join(home, "out.bin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ResolveDest(tc.dest, tc.src)
			if err != nil {
				t.Fatalf("ResolveDest: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveDest(%q,%q) = %q, want %q", tc.dest, tc.src, got, tc.want)
			}
		})
	}
}

// TestResolveTUIEndpoint covers how a delegated copy resolves endpoints against
// the originating instance: self is the sender, a bare instance defaults to the
// sender's fleet, and a plain path is local to this (the TUI's) machine.
func TestResolveTUIEndpoint(t *testing.T) {
	cases := []struct {
		arg  string
		want fleetclient.ResolvedEndpoint
	}{
		{":build/out", fleetclient.ResolvedEndpoint{Fleet: "myfleet", Instance: "self", Path: "build/out"}},
		{"other:/tmp/x", fleetclient.ResolvedEndpoint{Fleet: "myfleet", Instance: "other", Path: "/tmp/x"}},
		{"two/other:/tmp/x", fleetclient.ResolvedEndpoint{Fleet: "two", Instance: "other", Path: "/tmp/x"}},
		{"report.csv", fleetclient.ResolvedEndpoint{Local: true, Path: "report.csv"}},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			got := resolveTUIEndpoint(tc.arg, "myfleet", "self")
			if got != tc.want {
				t.Errorf("resolveTUIEndpoint(%q) = %+v, want %+v", tc.arg, got, tc.want)
			}
		})
	}
}

package cli

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// TestResolveHostEndpoint covers the host-form endpoint resolution: a plain path
// stays local, an explicit fleet/instance passes through, and `:path` (self) is
// rejected because a host invocation has no current instance. A bare instance
// (which infers the fleet from the cwd git remote) is exercised by integration
// tests, not here.
func TestResolveHostEndpoint(t *testing.T) {
	cases := []struct {
		arg  string
		want fleetclient.ResolvedEndpoint
	}{
		{"./tool", fleetclient.ResolvedEndpoint{Local: true, Path: "./tool"}},
		{"/abs/tool", fleetclient.ResolvedEndpoint{Local: true, Path: "/abs/tool"}},
		{"tool.txt", fleetclient.ResolvedEndpoint{Local: true, Path: "tool.txt"}},
		{"", fleetclient.ResolvedEndpoint{Local: true, Path: ""}},
		{"myfleet/alpha:/bin/tool", fleetclient.ResolvedEndpoint{Fleet: "myfleet", Instance: "alpha", Path: "/bin/tool"}},
		{"myfleet/alpha:rel/path", fleetclient.ResolvedEndpoint{Fleet: "myfleet", Instance: "alpha", Path: "rel/path"}},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			got, err := resolveHostEndpoint(tc.arg)
			if err != nil {
				t.Fatalf("resolveHostEndpoint(%q): %v", tc.arg, err)
			}
			if got != tc.want {
				t.Errorf("resolveHostEndpoint(%q) = %+v, want %+v", tc.arg, got, tc.want)
			}
		})
	}
}

// TestResolveHostEndpointRejectsSelf confirms `:path` errors on a host, where
// there is no current instance to mean.
func TestResolveHostEndpointRejectsSelf(t *testing.T) {
	if _, err := resolveHostEndpoint(":build/tool"); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("self on host: want error mentioning inside-an-instance, got %v", err)
	}
}

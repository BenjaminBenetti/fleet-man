package cli

import (
	"path/filepath"
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
		// `host:` is the same machine as a plain path on the host.
		{"host:/tmp/x", fleetclient.ResolvedEndpoint{Local: true, Path: "/tmp/x"}},
		{"host:rel.txt", fleetclient.ResolvedEndpoint{Local: true, Path: "rel.txt"}},
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

// TestRewriteInstanceLocal confirms that a plain (this-instance) path typed
// inside an instance is rewritten to an absolute `:` self endpoint, while
// host:/instance:/: forms and the empty 1-arg dst pass through unchanged.
func TestRewriteInstanceLocal(t *testing.T) {
	mustAbs := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("Abs(%q): %v", p, err)
		}
		return abs
	}
	cases := []struct{ arg, want string }{
		{"foo.txt", ":" + mustAbs("foo.txt")},
		{"./sub/bar", ":" + mustAbs("./sub/bar")},
		{"/abs/already", ":/abs/already"},
		{"", ""},
		{"host:/tmp/x", "host:/tmp/x"},
		{"alpha:/p", "alpha:/p"},
		{":build/out", ":build/out"},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			if got := rewriteInstanceLocal(tc.arg); got != tc.want {
				t.Errorf("rewriteInstanceLocal(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

// TestRequireDownloadSource enforces the 1-arg shorthand: a lone source must be a
// remote (instance/self) endpoint; a path on your own machine has no destination.
func TestRequireDownloadSource(t *testing.T) {
	// With a destination, anything is allowed.
	if err := requireDownloadSource("host:/tmp/x", "alpha:/p"); err != nil {
		t.Fatalf("2-arg should never error: %v", err)
	}
	// 1-arg remote sources are valid (download to downloads folder).
	for _, src := range []string{"alpha:/bin/tool", ":build/out", "myfleet/beta:/p"} {
		if err := requireDownloadSource(src, ""); err != nil {
			t.Errorf("1-arg %q should be allowed, got %v", src, err)
		}
	}
	// 1-arg own-machine sources are rejected.
	for _, src := range []string{"./file", "plain.txt", "host:/tmp/x", "/abs/file"} {
		if err := requireDownloadSource(src, ""); err == nil {
			t.Errorf("1-arg %q should require a destination", src)
		}
	}
}

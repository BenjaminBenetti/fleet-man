package serviceimages

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// Every default is pinned by digest; a connection's override wins, a blank
// one does not.
func TestFor(t *testing.T) {
	for _, img := range []string{DebCache, ImageCache, BuildkitRootless, Buildkit} {
		if !strings.Contains(img, "@sha256:") {
			t.Errorf("%s is not pinned", img)
		}
	}
	var rec fleet.KubeCluster
	for _, tt := range []struct {
		svc     fleet.SharedService
		rootful bool
		want    string
	}{
		{fleet.ServiceDebCache, false, DebCache}, {fleet.ServiceImageCache, false, ImageCache},
		{fleet.ServiceBuildkit, false, BuildkitRootless}, {fleet.ServiceBuildkit, true, Buildkit},
		{fleet.SharedService("nope"), false, ""},
	} {
		if got := For(&rec, tt.svc, tt.rootful); got != tt.want {
			t.Errorf("For(%s, rootful %v) = %q, want %q", tt.svc, tt.rootful, got, tt.want)
		}
	}
	rec.ServiceImages.BuildkitRootless, rec.ServiceImages.Buildkit = "own/rootless@sha256:1", "  "
	if got := For(&rec, fleet.ServiceBuildkit, false); got != "own/rootless@sha256:1" {
		t.Errorf("the override: %q", got)
	}
	if got := For(&rec, fleet.ServiceBuildkit, true); got != Buildkit {
		t.Errorf("a blank override: %q", got)
	}
}

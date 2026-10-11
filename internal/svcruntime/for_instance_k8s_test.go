//go:build !nok8s

package svcruntime

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/kubert"
)

// A Kubernetes instance's runtime is its connection's: one per connection,
// keyed by it, so a sweep runs each cluster on its own.
func TestForInstanceUsesTheInstancesCluster(t *testing.T) {
	a := ForInstance(&fleet.Instance{Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "aaaaaaaa"})
	b := ForInstance(&fleet.Instance{Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "bbbbbbbb"})
	other := ForCluster("c2")
	if a == nil || b == nil || other == nil {
		t.Fatalf("runtimes %v %v %v", a, b, other)
	}
	if a.Key() != kubert.Key("c1") || a.Key() != b.Key() || other.Key() == a.Key() {
		t.Fatalf("keys %q %q %q", a.Key(), b.Key(), other.Key())
	}
	if a.ReconfigureOnStart() {
		t.Error("a cluster's runtime reconfigures on start: its Services and NetworkPolicies outlive a stop")
	}
}

//go:build nok8s

package svcruntime

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// A build without the Kubernetes backend has no cluster runtime.
func TestForInstanceHasNoClusterRuntimeWithoutKubernetes(t *testing.T) {
	if rt := ForInstance(&fleet.Instance{Backend: fleet.BackendKubernetes, KubeCluster: "c1"}); rt != nil {
		t.Fatalf("runtime %q", rt.Key())
	}
	if got := PresentClusters("alpha"); len(got) != 0 {
		t.Fatalf("present clusters %v", got)
	}
}

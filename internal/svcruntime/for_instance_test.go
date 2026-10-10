package svcruntime

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/dockerrt"
)

// TestForInstanceByBackend pins which instances get the daemon host's docker
// for their shared services. Every backend type this build knows needs a row,
// so a new backend type has to decide here (it gets no shared services until it
// does).
func TestForInstanceByBackend(t *testing.T) {
	usesDocker := map[fleet.BackendType]bool{
		fleet.BackendDevcontainer: true,
		fleet.BackendCodespaces:   false,
		fleet.BackendCoder:        false,
		// A cluster's pods can't reach the daemon host's docker; they get no
		// shared services until their cluster has a runtime of its own.
		fleet.BackendKubernetes: false,
	}
	for _, backendType := range fleet.BackendTypes() {
		want, ok := usesDocker[backendType]
		if !ok {
			t.Errorf("backend %q has no row: decide which runtime its shared services use", backendType)
			continue
		}
		if got := ForInstance(&fleet.Instance{Backend: backendType}) == Docker(); got != want {
			t.Errorf("ForInstance(%q) uses docker = %v, want %v", backendType, got, want)
		}
	}
	if rt := ForInstance(&fleet.Instance{Backend: fleet.BackendKubernetes, KubeCluster: "c"}); rt != nil {
		t.Errorf("a Kubernetes instance got runtime %q", rt.Key())
	}
	// A record from before the Backend field is a devcontainer.
	if ForInstance(&fleet.Instance{}) != Docker() {
		t.Error(`ForInstance("") is not docker`)
	}
	if rt := ForInstance(&fleet.Instance{Backend: "some-future-backend"}); rt != nil {
		t.Errorf("an unknown backend got runtime %q", rt.Key())
	}
}

func TestDockerIsTheDockerRuntime(t *testing.T) {
	if got := Docker().Key(); got != dockerrt.Key {
		t.Fatalf("Docker().Key() = %q, want %q", got, dockerrt.Key)
	}
}

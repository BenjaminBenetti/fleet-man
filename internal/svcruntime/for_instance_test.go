package svcruntime

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/dockerrt"
)

// TestForInstanceByBackend pins which instances get the daemon host's docker
// for their shared services. Every deploy target this build knows needs a row,
// so a new backend type has to decide here (it gets no shared services until it
// does).
func TestForInstanceByBackend(t *testing.T) {
	usesDocker := map[fleet.BackendType]bool{
		fleet.BackendDevcontainer: true,
		fleet.BackendCodespaces:   false,
		fleet.BackendCoder:        false,
	}
	for _, target := range fleet.DeployTargets() {
		want, ok := usesDocker[target.Backend]
		if !ok {
			t.Errorf("backend %q has no row: decide which runtime its shared services use", target.Backend)
			continue
		}
		if got := ForInstance(&fleet.Instance{Backend: target.Backend}) == Docker(); got != want {
			t.Errorf("ForInstance(%q) uses docker = %v, want %v", target.Backend, got, want)
		}
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

package conformance

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/backendtest"
)

// targets builds the conformance target for a selected backend, by its
// FLEET_CONFORMANCE name. Each backend's file registers its own.
var targets = map[string]func(t *testing.T, sel backendtest.Selection) backendtest.Target{}

// TestConformance runs the suite against every backend FLEET_CONFORMANCE
// selects, one subtest per entry.
func TestConformance(t *testing.T) {
	sels, err := backendtest.Selected()
	if err != nil {
		t.Fatal(err)
	}
	if len(sels) == 0 {
		t.Skipf("set %s (e.g. %s=%s) to run the backend conformance suite", backendtest.EnvVar, backendtest.EnvVar, backendtest.BackendDevcontainer)
	}
	for _, sel := range sels {
		t.Run(sel.String(), func(t *testing.T) {
			build, ok := targets[sel.Backend]
			if !ok {
				t.Fatalf("this build has no conformance target for %s", sel.Backend)
			}
			backendtest.Run(t, build(t, sel))
		})
	}
}

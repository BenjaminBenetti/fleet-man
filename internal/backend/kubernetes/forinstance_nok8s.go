//go:build nok8s

package kubernetes

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// errNoKubernetes is every answer of a build without the backend.
var errNoKubernetes = kubeerr.New(kubeerr.Unavailable, "this build of fleet has no Kubernetes backend")

// ForInstance is the backend handle of a Kubernetes instance. This build
// (-tags nok8s, the in-pod one) has no Kubernetes backend: every method
// fails, saying so.
func ForInstance(*fleet.Instance) backend.Backend { return unavailable{err: errNoKubernetes} }

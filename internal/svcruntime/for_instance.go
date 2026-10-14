package svcruntime

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/dockerrt"
)

// docker is the daemon host's docker runtime.
var docker Runtime = dockerrt.Runtime{}

// Docker returns the runtime that runs shared services as containers on the
// daemon host's docker.
func Docker() Runtime { return docker }

// ForInstance returns the runtime that runs inst's fleet's shared services for
// inst, or nil when inst can't use any. A devcontainer (an empty Backend is a
// devcontainer, as everywhere) uses Docker: it runs on that docker and can join
// its networks and bind its sockets. A Kubernetes instance uses its
// connection's runtime (ForCluster), which runs the services in the
// connection's namespace beside its pods; a record that names no connection
// gets none. A Codespaces or Coder workspace runs on someone else's machine
// and reaches neither, so it gets nil, as does a backend type this build does
// not know (the Kubernetes one in a build without it). It does no I/O, so it is
// safe under the state lock.
func ForInstance(inst *fleet.Instance) Runtime {
	switch inst.Backend {
	case "", fleet.BackendDevcontainer:
		return docker
	case fleet.BackendKubernetes:
		return ForCluster(inst.KubeCluster)
	default:
		return nil
	}
}

// ForCluster returns the runtime of Kubernetes connection clusterID, or nil
// for no connection (and in a build without the Kubernetes backend). Each
// connection is a runtime of its own (its Key names it). No I/O.
func ForCluster(clusterID string) Runtime {
	if clusterID == "" {
		return nil
	}
	return kubeRuntime(clusterID)
}

// PresentClusters are the Kubernetes connections the daemon recorded
// fleetName's objects on (~/.fleet/kube/footprint.json): where its cache
// volumes may still be after its instances there left. Empty in a build
// without the Kubernetes backend.
func PresentClusters(fleetName string) []string { return presentClusters(fleetName) }

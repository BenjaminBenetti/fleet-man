package tui

import "github.com/BenjaminBenetti/fleet-man/fleetgrpc"

// kubeRowNote is the note a running Kubernetes instance's row carries after
// its status: the restart the cluster made on its own, until the next attach
// clears it ("sessions reset: node drained"). An out-of-memory restart, which
// killed every process and not only the sessions, says so. "" when there is
// none (and for every other backend, which has no InstanceRuntime.kube).
func kubeRowNote(rt *fleetgrpc.InstanceRuntime) string {
	k := rt.GetKube()
	switch {
	case k.GetRestartCause() == "":
		return ""
	case k.GetSessionsEnded():
		return "restarted: out of memory, sessions ended"
	}
	return "sessions reset: " + k.GetRestartCause()
}

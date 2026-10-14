//go:build nok8s

package tui

import (
	"errors"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// kubeLoginHereCommand: the -tags nok8s pod build links no client-go, so it
// cannot read a kubeconfig to run a login plugin.
func kubeLoginHereCommand(*fleetgrpc.KubeCluster) (*exec.Cmd, error) {
	return nil, errors.New("this fleet build cannot run Kubernetes login plugins")
}

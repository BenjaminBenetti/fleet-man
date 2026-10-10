//go:build !nok8s

package tui

import (
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// kubeLoginHereCommand is connection c's login plugin, run in this TUI's
// terminal: [L] Run login here. It reads the kubeconfig files c names on THIS
// machine, so it is offered only over the local socket, where this machine is
// the daemon host and a sign-in lands in the token caches fleetd reads. A
// remote TUI uses [ login on fleetd ] instead, which runs the plugin where the
// kubeconfig lives and relays only the device code.
func kubeLoginHereCommand(c *fleetgrpc.KubeCluster) (*exec.Cmd, error) {
	conn, err := kubeexec.Load(protoconv.KubeClusterFromProto(c), kubeexec.Options{Environ: os.Environ()})
	if err != nil {
		return nil, err
	}
	return conn.LoginCommand()
}

//go:build !linux

package podinit

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// PodUIDEnv carries the pod's uid to pod-init.
const PodUIDEnv = "FLEET_POD_UID"

// Run fails off linux: pod-init is PID 1 of a linux pod.
func Run(_ string, _, stderr *os.File) int {
	fmt.Fprintln(stderr, "fleet pod-init: runs only as PID 1 of a linux pod")
	return execas.ExitFailed
}

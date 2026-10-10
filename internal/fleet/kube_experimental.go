package fleet

import (
	"os"
	"strings"
)

// EnvExperimentalK8s turns on the Kubernetes deploy targets while the
// Kubernetes backend is still being built: the daemon offers one per
// connection only when it runs with this set ("1", "true", ...). Connections
// themselves (Settings, `fleet cluster`) are always available.
const EnvExperimentalK8s = "FLEET_EXPERIMENTAL_K8S"

// ExperimentalK8s reports whether EnvExperimentalK8s is on in this process:
// set, and not "0", "false", "no" or "off".
func ExperimentalK8s() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvExperimentalK8s))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

package fleet

import "fmt"

// BackendType identifies which backend an instance uses.
type BackendType string

const (
	BackendDevcontainer BackendType = "devcontainer"
	BackendCoder        BackendType = "coder"
	BackendCodespaces   BackendType = "codespaces"
	// BackendKubernetes runs an instance as a pod on a cluster connection
	// (KubeCluster), which the record names in Instance.KubeCluster.
	BackendKubernetes BackendType = "kubernetes"
)

// backendTypes lists every backend type this build knows.
var backendTypes = []BackendType{BackendDevcontainer, BackendCoder, BackendCodespaces, BackendKubernetes}

// BackendTypes returns every backend type this build knows, including the
// ones ParseBackendType does not accept for a new instance yet.
func BackendTypes() []BackendType {
	return append([]BackendType(nil), backendTypes...)
}

// ParseBackendType validates a backend named for a new instance (the CLI's
// --backend, MCP fleet_up, an automation agent, a create) and returns its
// BackendType. Kubernetes is a known type, so its records and requests travel
// and persist, but not one a create can name yet: this build cannot run
// instances on a cluster.
func ParseBackendType(value string) (BackendType, error) {
	switch BackendType(value) {
	case BackendDevcontainer:
		return BackendDevcontainer, nil
	case BackendCoder:
		return BackendCoder, nil
	case BackendCodespaces:
		return BackendCodespaces, nil
	default:
		return "", fmt.Errorf("invalid backend %q (valid: devcontainer, coder, codespaces)", value)
	}
}

// ValidateBackendType returns an error if a new instance cannot use
// backendType (see ParseBackendType).
func ValidateBackendType(backendType BackendType) error {
	_, err := ParseBackendType(string(backendType))
	return err
}

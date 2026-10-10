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
// ones ValidateBackendType does not let a new instance use yet.
func BackendTypes() []BackendType {
	return append([]BackendType(nil), backendTypes...)
}

// ParseBackendType validates a backend named for a new instance (the CLI's
// --backend, MCP fleet_up, an automation agent, a create) and returns its
// BackendType. Every known type parses; whether this daemon can run a new
// instance of it is ValidateBackendType's question.
func ParseBackendType(value string) (BackendType, error) {
	switch BackendType(value) {
	case BackendDevcontainer:
		return BackendDevcontainer, nil
	case BackendCoder:
		return BackendCoder, nil
	case BackendCodespaces:
		return BackendCodespaces, nil
	case BackendKubernetes:
		return BackendKubernetes, nil
	default:
		return "", fmt.Errorf("invalid backend %q (valid: devcontainer, coder, codespaces, kubernetes)", value)
	}
}

// ErrKubernetesExperimental refuses a new Kubernetes instance on a daemon
// that does not run with FLEET_EXPERIMENTAL_K8S while the backend is being
// built.
var ErrKubernetesExperimental = fmt.Errorf("kubernetes instances are experimental: start fleetd with %s=1 to create them", EnvExperimentalK8s)

// ValidateBackendType returns an error if a new instance cannot use
// backendType in this process: a type ParseBackendType refuses, or
// Kubernetes while EnvExperimentalK8s is off (the daemon decides; a client
// never refuses it on its own behalf).
func ValidateBackendType(backendType BackendType) error {
	if _, err := ParseBackendType(string(backendType)); err != nil {
		return err
	}
	if backendType == BackendKubernetes && !ExperimentalK8s() {
		return ErrKubernetesExperimental
	}
	return nil
}

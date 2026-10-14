package conformance

import (
	"os"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/backendtest"
	"github.com/BenjaminBenetti/fleet-man/internal/testhome"
)

// TestMain moves a selected run into a HOME of its own before any backend
// runs. docker's client config (contexts, credential helpers) stays reachable
// through DOCKER_CONFIG, and so do the kubeconfig (KUBECONFIG) and minikube's
// profiles (MINIKUBE_HOME), which a cluster run reaches its cluster with.
func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv(backendtest.EnvVar)) == "" {
		os.Exit(m.Run())
	}
	testhome.Main(m)
}

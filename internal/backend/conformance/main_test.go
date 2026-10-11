package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/backendtest"
)

// TestMain moves a selected run into a HOME of its own before any backend
// runs. docker's client config (contexts, credential helpers) stays reachable
// through DOCKER_CONFIG, and so do the kubeconfig (KUBECONFIG) and minikube's
// profiles (MINIKUBE_HOME), which a cluster run reaches its cluster with.
func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv(backendtest.EnvVar)) == "" {
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "fleet-conformance-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if real, err := os.UserHomeDir(); err == nil {
		for _, keep := range []struct{ env, path string }{
			{"DOCKER_CONFIG", ".docker"},
			{"KUBECONFIG", filepath.Join(".kube", "config")},
			{"MINIKUBE_HOME", ".minikube"},
		} {
			if os.Getenv(keep.env) != "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(real, keep.path)); err == nil {
				_ = os.Setenv(keep.env, filepath.Join(real, keep.path))
			}
		}
	}
	_ = os.Setenv("HOME", home)
	code := m.Run()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "leaving %s: %v\n", home, err)
	}
	os.Exit(code)
}

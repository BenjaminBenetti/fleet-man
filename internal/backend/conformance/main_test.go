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
// through DOCKER_CONFIG.
func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv(backendtest.EnvVar)) == "" {
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "fleet-conformance-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if os.Getenv("DOCKER_CONFIG") == "" {
		if real, err := os.UserHomeDir(); err == nil {
			if info, err := os.Stat(filepath.Join(real, ".docker")); err == nil && info.IsDir() {
				_ = os.Setenv("DOCKER_CONFIG", filepath.Join(real, ".docker"))
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

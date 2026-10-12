// Package testhome gives a package's tests a HOME of their own, so `go test`
// never writes the invoking user's ~/.fleet (state, logs, locks, cluster
// records) or anything else under their home. A package whose tests reach
// the daemon's state calls it from TestMain:
//
//	func TestMain(m *testing.M) { testhome.Main(m) }
//
// Tests that set HOME themselves (t.Setenv) still do; what this catches is
// the test that forgets, and work a test leaves running (a goroutine, a job)
// that writes after the test's own HOME was handed back. Only test files
// import it.
package testhome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// kept are the tool configurations a live test reaches its docker daemon,
// cluster or minikube profile with: they stay where they are, named by
// their environment variables, unless the caller set those itself.
var kept = []struct{ env, path string }{
	{"DOCKER_CONFIG", ".docker"},
	{"KUBECONFIG", filepath.Join(".kube", "config")},
	{"MINIKUBE_HOME", ".minikube"},
}

// Main runs m with HOME set to a new directory (removed afterwards) and exits
// with m's code.
func Main(m *testing.M) {
	os.Exit(Run(m))
}

// Run is Main without the exit, for a TestMain that does more around it.
func Run(m *testing.M) int {
	home, err := os.MkdirTemp("", "fleet-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		return 1
	}
	restore := Use(home)
	code := m.Run()
	// Removed before HOME is handed back, so work the tests left running
	// writes into home while it goes, not into the real one (Main exits
	// right after).
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "testhome: leaving %s: %v\n", home, err)
	}
	restore()
	return code
}

// Use points HOME at home, keeping the tool configurations of the real one
// reachable (kept), and returns the function that puts the environment back.
func Use(home string) (restore func()) {
	var undo []func()
	set := func(key, value string) {
		old, had := os.LookupEnv(key)
		_ = os.Setenv(key, value)
		undo = append(undo, func() {
			if had {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if real, err := os.UserHomeDir(); err == nil {
		for _, k := range kept {
			if os.Getenv(k.env) != "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(real, k.path)); err == nil {
				set(k.env, filepath.Join(real, k.path))
			}
		}
	}
	set("HOME", home)
	return func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
}

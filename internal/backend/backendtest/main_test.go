package backendtest

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package's tests a HOME of their own before anything logs:
// fleet.log (which the exec-quiet case reads) then lands there, never in the
// developer's ~/.fleet.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "backendtest-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

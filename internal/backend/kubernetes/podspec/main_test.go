package podspec

import (
	"os"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// TestMain makes the test binary multicall like cmd/fleet, so the capture-up
// TestRecordCaptures runs reaches the real shim through the docker link the
// shim directory points at the test binary.
func TestMain(m *testing.M) {
	if kubeshim.Invoked(os.Args[0]) {
		os.Exit(kubeshim.Main())
	}
	os.Exit(m.Run())
}

package devcli

import (
	"os"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// TestMain makes the test binary multicall like cmd/fleet: the shim
// directories the tests create link docker and docker-compose to it
// (Tools.Exe defaults to os.Executable()), so the CLI's docker calls run the
// real shim code.
func TestMain(m *testing.M) {
	if kubeshim.Invoked(os.Args[0]) {
		os.Exit(kubeshim.Main())
	}
	os.Exit(m.Run())
}

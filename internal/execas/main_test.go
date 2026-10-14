package execas

import (
	"os"
	"testing"
)

// helperEnv names the helper role a re-executed test binary plays (see
// runTestHelper); the tests run it as the commands they start.
const helperEnv = "EXECAS_TEST_HELPER"

func TestMain(m *testing.M) {
	if role := os.Getenv(helperEnv); role != "" {
		runTestHelper(role)
		os.Exit(2) // a helper exits on its own
	}
	os.Exit(m.Run())
}

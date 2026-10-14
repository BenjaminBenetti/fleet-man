package backendtest

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/testhome"
)

// TestMain gives the package's tests a HOME of their own before anything logs:
// fleet.log (which the exec-quiet case reads) then lands there, never in the
// developer's ~/.fleet.
func TestMain(m *testing.M) { testhome.Main(m) }

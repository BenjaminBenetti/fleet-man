package server

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/testhome"
)

// TestMain gives the package's tests a HOME of their own: a test that does
// not set one, or work a test leaves running past its own HOME, writes
// there, never into the invoking user's ~/.fleet.
func TestMain(m *testing.M) { testhome.Main(m) }

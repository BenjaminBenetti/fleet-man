package state

import (
	"os"
	"strings"
	"testing"
)

// TestSaveStateAtomicNoTornReads asserts repeated Save round-trips (which now go
// through atomicfile.Write) keep state.json well-formed and complete — the
// property the backup loop relies on when it reads the file unlocked.
func TestSaveStateAtomicNoTornReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for i := range 5 {
		if err := Save(&State{}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		// An unlocked read (as the backup loop does) must always see a complete file.
		data, err := os.ReadFile(StatePath())
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(data) == 0 || data[len(data)-1] != '}' {
			t.Fatalf("state.json looks truncated on iter %d: %q", i, data)
		}
	}
	// The atomic writer must not leave temp files in ~/.fleet.
	entries, _ := os.ReadDir(FleetDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file in fleet dir: %s", e.Name())
		}
	}
}

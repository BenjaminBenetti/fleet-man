package kubeshim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A shim directory a crashed daemon left (holding the capture's
// environment) is removed once it is old; a young one, and anything else in
// the temp dir, is left.
func TestSweepStale(t *testing.T) {
	// Short enough that NewDir uses it: macOS's t.TempDir() is too deep for
	// the exec socket, and NewDir falls back to /tmp there
	// (TestNewDirUnderADeepTempDir).
	tmp := shortTempDir(t)
	t.Setenv("TMPDIR", tmp)
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(tmp, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "capture.raw.json"), []byte(`{"env":"secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := mk(dirPrefix+"1111111111", 48*time.Hour)
	young := mk(dirPrefix+"2222222222", time.Minute)
	other := mk("something-else", 48*time.Hour)
	if n := sweepStaleIn([]string{tmp}, 24*time.Hour, time.Now()); n != 1 {
		t.Errorf("removed %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the stale shim dir is still there: %v", err)
	}
	for _, p := range []string{young, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	// NewDir names its directories so the sweep finds them.
	d, err := NewDir("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove()
	if filepath.Dir(d.Path()) != tmp || filepath.Base(d.Path())[:len(dirPrefix)] != dirPrefix {
		t.Errorf("NewDir made %s", d.Path())
	}
}

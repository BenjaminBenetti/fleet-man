package testhome

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

// TestHelperProcess is the helper TestAHelperProcessKeepsItsParentsHome
// starts: like most helpers, it ends in os.Exit.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("FLEET_TESTHOME_HELPER") != "1" {
		return
	}
	os.Exit(0)
}

// A test binary a test starts again runs in that test's HOME and makes no
// home of its own, so a helper that ends in os.Exit leaves nothing in the
// temp dir.
func TestAHelperProcessKeepsItsParentsHome(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, homePrefix+"parent")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "FLEET_TESTHOME_HELPER=1", "HOME="+home, "TMPDIR="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the helper: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != homePrefix+"parent" {
		t.Errorf("the temp dir after the helper holds %v, want only the parent's home", names)
	}
}

func TestInherited(t *testing.T) {
	tmp := os.TempDir()
	for _, tc := range []struct {
		home string
		want bool
	}{
		{filepath.Join(tmp, homePrefix+"123"), true},
		{filepath.Join(tmp, homePrefix+"123") + "/", true},
		{filepath.Join(tmp, "other", homePrefix+"123"), false},
		{filepath.Join(tmp, "home"), false},
		{"/home/someone", false},
		{"", false},
	} {
		t.Setenv("HOME", tc.home)
		if got := inherited(); got != tc.want {
			t.Errorf("HOME=%q: inherited %v, want %v", tc.home, got, tc.want)
		}
	}
}

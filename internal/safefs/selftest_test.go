//go:build unix

package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfTestPasses runs the walker self-test sharesync gates on, with each
// walk, and checks it leaves nothing behind.
func TestSelfTestPasses(t *testing.T) {
	eachWalk(t, func(t *testing.T, portable bool) {
		dir := t.TempDir()
		if err := selfTest(dir, Options{}, portable); err != nil {
			t.Fatal(err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("self-test left %v, %v", entries, err)
		}
	})
	if err := SelfTest(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// TestSelfTestFailsWhenAPromiseBreaks checks the self-test reports a broken
// guarantee, naming the check: with the link it removes last reserved, that
// remove is refused.
func TestSelfTestFailsWhenAPromiseBreaks(t *testing.T) {
	err := selfTest(t.TempDir(), Options{Reserved: []string{"abs"}}, true)
	if err == nil || !strings.Contains(err.Error(), "safefs self-test: remove a link, not its target: ") {
		t.Fatalf("selfTest with abs reserved = %v, want the remove check to fail", err)
	}
}

// TestSelfTestScratchIsATempName checks scanners skip the scratch folder.
func TestSelfTestScratchIsATempName(t *testing.T) {
	dir := t.TempDir()
	scratch, err := os.MkdirTemp(dir, TempPrefix+"selftest-*"+tempSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if !IsTemp(filepath.Base(scratch)) {
		t.Errorf("IsTemp(%q) = false", filepath.Base(scratch))
	}
}

package state

import (
	"os"
	"strings"
	"testing"
)

// (fleet.log gets only the new warning; the banner file gets both — see
// PrependWarn.)
func TestPrependWarnKeepsExistingBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// No existing banner: plain write.
	PrependWarn("f", "i", "first")
	got, err := os.ReadFile(WarnPath("f", "i"))
	if err != nil || string(got) != "first" {
		t.Fatalf("banner = %q, %v; want %q", got, err, "first")
	}

	// Existing banner: the new warning leads, the old text survives.
	WriteWarn("f", "i", "dotfiles failed\ndetails")
	PrependWarn("f", "i", "template warning")
	got, _ = os.ReadFile(WarnPath("f", "i"))
	lines := strings.Split(string(got), "\n")
	if lines[0] != "template warning" {
		t.Fatalf("first line = %q, want the prepended warning", lines[0])
	}
	if !strings.Contains(string(got), "dotfiles failed\ndetails") {
		t.Fatalf("existing banner lost: %q", got)
	}
}

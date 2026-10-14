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

// TestReplaceWarnLine: a line by prefix goes in front, moves when its text
// changes (once, not twice), leaves the rest alone, and is taken off again;
// a banner left with nothing goes.
func TestReplaceWarnLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const prefix = "Buildkit credential "
	read := func() string {
		got, err := os.ReadFile(WarnPath("f", "i"))
		if os.IsNotExist(err) {
			return "<none>"
		}
		return string(got)
	}

	if !ReplaceWarnLine("f", "i", prefix, prefix+"expires in 40h") || read() != prefix+"expires in 40h" {
		t.Fatalf("on a missing banner: %q", read())
	}
	WriteWarn("f", "i", "deb cache: failed\n"+prefix+"expires in 40h")
	if !ReplaceWarnLine("f", "i", prefix, prefix+"expires in 39h") || read() != prefix+"expires in 39h\ndeb cache: failed" {
		t.Fatalf("moved: %q", read())
	}
	if ReplaceWarnLine("f", "i", prefix, prefix+"expires in 39h") {
		t.Fatal("the same line changed the banner")
	}
	if !ReplaceWarnLine("f", "i", prefix, "") || read() != "deb cache: failed" {
		t.Fatalf("taken off: %q", read())
	}
	if ReplaceWarnLine("f", "i", prefix, "") {
		t.Fatal("taking off a line not there changed the banner")
	}
	WriteWarn("f", "i", prefix+"expired 2h ago")
	if !ReplaceWarnLine("f", "i", prefix, "") || read() != "<none>" {
		t.Fatalf("a banner left empty: %q", read())
	}
}

package fleet

import (
	"slices"
	"testing"
)

// TestNormalizeCustomMountValid checks that legal absolute paths are accepted
// and canonicalized (trimmed + filepath.Clean'd).
func TestNormalizeCustomMountValid(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/opt/data", "/opt/data"},
		{"  /opt/data  ", "/opt/data"},   // whitespace trimmed
		{"/opt/data/", "/opt/data"},      // trailing slash collapsed
		{"/opt//data", "/opt/data"},      // double slash collapsed
		{"/opt/./data", "/opt/data"},     // "." segment collapsed
		{"/var/cache/shared", "/var/cache/shared"},
		{"/srv", "/srv"},
	}
	for _, tc := range cases {
		got, err := NormalizeCustomMount(tc.in)
		if err != nil {
			t.Errorf("NormalizeCustomMount(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeCustomMount(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeCustomMountInvalid checks that empty, relative, root, and
// traversal paths are rejected — the traversal cases are the security boundary.
func TestNormalizeCustomMountInvalid(t *testing.T) {
	bad := []string{
		"",                // empty
		"   ",             // whitespace only
		"relative/path",   // not absolute
		"opt/data",        // not absolute
		"/",               // container root
		"/opt/../../etc",  // explicit traversal escaping .mnt
		"/../etc",         // traversal at the root
		"/opt/..",         // trailing traversal
		"..",              // bare traversal, also relative
	}
	for _, in := range bad {
		if got, err := NormalizeCustomMount(in); err == nil {
			t.Errorf("NormalizeCustomMount(%q) = %q, want error", in, got)
		}
	}
}

// TestNormalizeCustomMountsDedup verifies the list normalizer canonicalizes,
// de-duplicates exact repeats (preserving first-seen order), and propagates the
// first validation error.
func TestNormalizeCustomMountsDedup(t *testing.T) {
	got, err := NormalizeCustomMounts([]string{
		"/opt/data",
		"/opt/data/", // duplicate after cleaning
		"/var/cache",
		" /opt/data ", // duplicate after trim
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"/opt/data", "/var/cache"}
	if !slices.Equal(got, want) {
		t.Fatalf("NormalizeCustomMounts() = %v, want %v", got, want)
	}
}

// TestNormalizeCustomMountsEmpty verifies nil/empty input yields a nil slice.
func TestNormalizeCustomMountsEmpty(t *testing.T) {
	got, err := NormalizeCustomMounts(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("NormalizeCustomMounts(nil) = %v, want nil", got)
	}
}

// TestNormalizeCustomMountsRejectsInvalidEntry verifies one bad entry fails the
// whole list (so an invalid mount can never be persisted).
func TestNormalizeCustomMountsRejectsInvalidEntry(t *testing.T) {
	if _, err := NormalizeCustomMounts([]string{"/ok", "../escape"}); err == nil {
		t.Fatal("expected error for list containing a traversal entry")
	}
}

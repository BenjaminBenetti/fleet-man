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
		{"  /opt/data  ", "/opt/data"}, // whitespace trimmed
		{"/opt/data/", "/opt/data"},    // trailing slash collapsed
		{"/opt//data", "/opt/data"},    // double slash collapsed
		{"/opt/./data", "/opt/data"},   // "." segment collapsed
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
		"",               // empty
		"   ",            // whitespace only
		"relative/path",  // not absolute
		"opt/data",       // not absolute
		"/",              // container root
		"/opt/../../etc", // explicit traversal escaping .mnt
		"/../etc",        // traversal at the root
		"/opt/..",        // trailing traversal
		"..",             // bare traversal, also relative
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

// TestNormalizeCustomMountHostFolder checks a host folder is absolute and
// cleaned, and never one docker's --mount value could not carry.
func TestNormalizeCustomMountHostFolder(t *testing.T) {
	for in, want := range map[string]string{
		"/data/big":       "/data/big",
		" /data/big/ ":    "/data/big",
		"/data//big/./x":  "/data/big/x",
		"/mnt/a b":        "/mnt/a b",
		"/data/../srv/ok": "/srv/ok",
	} {
		got, err := NormalizeCustomMountHostFolder(in)
		if err != nil || got != want {
			t.Errorf("NormalizeCustomMountHostFolder(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "data", "~/data", "/", "/..", "/a,b", "/a\"b", "/a\nb", "/a\x00b"} {
		if got, err := NormalizeCustomMountHostFolder(in); err == nil {
			t.Errorf("NormalizeCustomMountHostFolder(%q) = %q, want an error", in, got)
		}
	}
}

// TestNormalizeCustomMountHostFolders keys each folder by its mount's
// normalized path, and drops cleared folders and ones of no custom mount.
func TestNormalizeCustomMountHostFolders(t *testing.T) {
	mounts := []string{"/opt/data", "/srv"}
	got, err := NormalizeCustomMountHostFolders(mounts, map[string]string{
		"/opt/data/": "/big/disk/",
		"/srv":       "",
		"/gone":      "/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["/opt/data"] != "/big/disk" {
		t.Errorf("got %v, want only /opt/data -> /big/disk", got)
	}
	if got, err := NormalizeCustomMountHostFolders(mounts, map[string]string{"/srv": ""}); err != nil || got != nil {
		t.Errorf("all cleared: got %v, %v; want nil", got, err)
	}
	if _, err := NormalizeCustomMountHostFolders(mounts, map[string]string{"/srv": "relative"}); err == nil {
		t.Error("a relative host folder was accepted")
	}
}

// TestSetAndRemoveCustomMount checks the shared mutators the CLI and MCP
// tools use: add, re-point, clear and remove, the mount list's order kept.
func TestSetAndRemoveCustomMount(t *testing.T) {
	s := FleetSettings{CustomMounts: []string{"/a"}}
	s, err := SetCustomMount(s, "/b/", "/big/b")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.CustomMounts, []string{"/a", "/b"}) || s.CustomMountHostFolders["/b"] != "/big/b" {
		t.Fatalf("after add: %+v", s)
	}
	if s, err = SetCustomMount(s, "/a", "/big/a"); err != nil || !slices.Equal(s.CustomMounts, []string{"/a", "/b"}) || s.CustomMountHostFolders["/a"] != "/big/a" {
		t.Fatalf("re-pointing an existing mount: %+v, %v", s, err)
	}
	if s, err = SetCustomMount(s, "/a", ""); err != nil || len(s.CustomMountHostFolders) != 1 {
		t.Fatalf("clearing a host folder: %+v, %v", s, err)
	}
	if _, err := SetCustomMount(s, "/c", "rel"); err == nil {
		t.Error("a relative host folder was set")
	}
	if s, err = RemoveCustomMount(s, "/b"); err != nil || !slices.Equal(s.CustomMounts, []string{"/a"}) || s.CustomMountHostFolders != nil {
		t.Fatalf("after remove: %+v, %v", s, err)
	}
	if _, err := RemoveCustomMount(s, "/nope"); err == nil {
		t.Error("removing a mount the fleet does not have succeeded")
	}
}

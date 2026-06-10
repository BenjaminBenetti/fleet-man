package tui

import (
	"path/filepath"
	"testing"
)

func TestResolveRequestedDest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		dest string
		want string
	}{
		{"empty means downloads folder", "", home}, // temp home has no Downloads dir
		{"absolute used as-is", "/abs/target", "/abs/target"},
		{"bare tilde is home", "~", home},
		{"tilde-slash resolves against home", "~/builds/tool", filepath.Join(home, "builds/tool")},
		{"relative resolves against home", "builds/tool", filepath.Join(home, "builds/tool")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRequestedDest(tc.dest); got != tc.want {
				t.Errorf("resolveRequestedDest(%q) = %q, want %q", tc.dest, got, tc.want)
			}
		})
	}
}

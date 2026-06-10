package fleetclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCopyDest(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		dest string
		want string
	}{
		{"empty dest keeps basename", "", "tool"},
		{"explicit file path wins", filepath.Join(dir, "renamed"), filepath.Join(dir, "renamed")},
		{"existing directory keeps basename", dir, filepath.Join(dir, "tool")},
		{"trailing separator keeps basename", dir + string(os.PathSeparator), filepath.Join(dir, "tool")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCopyDest(tc.dest, "tool")
			if err != nil {
				t.Fatalf("ResolveCopyDest(%q): %v", tc.dest, err)
			}
			if got != tc.want {
				t.Errorf("ResolveCopyDest(%q) = %q, want %q", tc.dest, got, tc.want)
			}
		})
	}

	if _, err := ResolveCopyDest(dir, ""); err == nil {
		t.Error("want error for empty server-sent name")
	}
}

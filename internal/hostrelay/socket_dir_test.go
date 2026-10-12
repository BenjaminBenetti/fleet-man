package hostrelay

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestListSockets: a recorded socket directory lists its sockets (only), by
// name, with their permission bits; a link put at the directory, or one that
// is not a directory, is refused.
func TestListSockets(t *testing.T) {
	dir := shortDir(t)
	for _, name := range []string{"X1", "X0"} {
		ln, err := net.Listen("unix", filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
	}
	if err := os.Chmod(filepath.Join(dir, "X1"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "X0"), filepath.Join(dir, "X2")); err != nil {
		t.Fatal(err)
	}
	got, err := ListSockets(dir)
	if err != nil {
		t.Fatal(err)
	}
	x0, err := os.Stat(filepath.Join(dir, "X0"))
	if err != nil {
		t.Fatal(err)
	}
	want := []DirSocket{{Name: "X0", Mode: x0.Mode().Perm()}, {Name: "X1", Mode: 0o660}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %+v, want %+v", got, want)
	}
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ListSockets(link); !errors.Is(err, ErrLink) {
		t.Fatalf("a link at the recorded directory: %v", err)
	}
	if _, err := ListSockets(filepath.Join(dir, "notes")); err == nil {
		t.Fatal("a file listed as a directory")
	}
	if _, err := ListSockets(filepath.Join(dir, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a directory that is gone: %v", err)
	}
	if _, err := ListSockets("rel/dir"); err == nil {
		t.Fatal("a relative path listed")
	}
}

// TestInDir: a target directly in a recorded directory, by a plain name the
// directory does not leave to another bind, is relayed to that name in its
// source; anything else is not one of its sockets.
func TestInDir(t *testing.T) {
	dirs := []SocketDir{{Target: "/tmp/.X11-unix", Source: "/host/x11", Shadowed: []string{"X9"}}, {Target: "/run/user/1000/gnupg", Source: "/host/gnupg"}}
	for target, want := range map[string]string{
		"/tmp/.X11-unix/X0":                "/host/x11/X0",
		"/run/user/1000/gnupg/S.gpg-agent": "/host/gnupg/S.gpg-agent",
		"/tmp/.X11-unix/X9":                "",
		"/tmp/.X11-unix/sub/X0":            "",
		"/tmp/.X11-unix/../etc/passwd":     "",
		"/tmp/.X11-unix":                   "",
		"/tmp/other/X0":                    "",
		"relative/X0":                      "",
		"":                                 "",
	} {
		got, ok := InDir(dirs, target)
		if got != want || ok != (want != "") {
			t.Errorf("InDir(%q) = %q %v, want %q", target, got, ok, want)
		}
	}
}

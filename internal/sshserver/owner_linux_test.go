package sshserver

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The owner of a listening socket is found from /proc: this test binary
// owns the sockets it listens on, so its directory is "VS Code's", and an
// unrelated directory is not.
func TestSocketOwnedBy(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	mine := []string{filepath.Dir(exe)}
	elsewhere := []string{t.TempDir()}

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	sock := filepath.Join(t.TempDir(), "srv.sock")
	unix, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()

	for _, tc := range []struct{ network, addr string }{{"tcp", tcp.Addr().String()}, {"unix", sock}} {
		if !socketOwnedBy(tc.network, tc.addr, mine) {
			t.Errorf("%s %s: not found to be this process's", tc.network, tc.addr)
		}
		if socketOwnedBy(tc.network, tc.addr, elsewhere) {
			t.Errorf("%s %s: owned by a directory nothing runs from", tc.network, tc.addr)
		}
	}
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	_ = free.Close()
	if socketOwnedBy("tcp", addr, mine) {
		t.Error("a closed port has an owner")
	}
	if socketOwnedBy("unix", filepath.Join(t.TempDir(), "none.sock"), mine) {
		t.Error("a missing socket has an owner")
	}
}

// A directory runs when a process's executable is under it: this test
// binary's directory does, an empty one does not.
func TestRunsFrom(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !runsFrom(filepath.Dir(exe)) {
		t.Errorf("%s: this process runs from it", filepath.Dir(exe))
	}
	if runsFrom(t.TempDir()) {
		t.Error("an empty directory runs")
	}
	if runsFrom(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing directory runs")
	}
}

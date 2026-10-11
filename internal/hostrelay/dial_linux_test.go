//go:build linux

package hostrelay

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestDialReachesTheSocketItChecked: a link put at the socket's name between
// Dial's check and its connect (connect(2) would follow it) is not followed:
// the connection reaches the socket that was checked.
func TestDialReachesTheSocketItChecked(t *testing.T) {
	dir := shortDir(t)
	recorded := filepath.Join(dir, "s")
	echoSocket(t, recorded)
	other := filepath.Join(dir, "other")
	ln, err := net.Listen("unix", other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "the other socket")
			_ = c.Close()
		}
	}()

	beforeConnect = func() {
		if err := os.Rename(recorded, filepath.Join(dir, "moved")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(other, recorded); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeConnect = nil })
	c, err := Dial(context.Background(), recorded)
	if err != nil {
		t.Fatal(err)
	}
	if got := talkOver(t, c, "sign"); got != "echo:sign" {
		t.Fatalf("got %q, want the checked socket's answer", got)
	}
}

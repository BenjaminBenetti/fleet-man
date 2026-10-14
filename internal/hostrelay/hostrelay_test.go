package hostrelay

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir is a scratch directory with a short path (unix socket addresses
// are capped near 104 bytes), resolved, so it holds no symbolic link (macOS's
// /tmp is one).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	real, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// echoSocket serves "echo:<what was read>" at path, half-closing after it.
func echoSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
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
			go func() {
				defer c.Close()
				in, _ := io.ReadAll(c)
				_, _ = c.Write([]byte("echo:" + string(in)))
			}()
		}
	}()
}

func talkOver(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	closeWrite(c)
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestDialReachesARecordedSocket: a recorded (resolved) path to a live
// socket is dialled, the half-close carried.
func TestDialReachesARecordedSocket(t *testing.T) {
	dir := shortDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "gnupg"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "gnupg", "S.gpg-agent.extra")
	echoSocket(t, sock)
	c, err := Dial(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := talkOver(t, c, "sign"); got != "echo:sign" {
		t.Fatalf("got %q", got)
	}
}

// TestDialRefusesWhatWasNotRecorded: Dial refuses a symbolic link anywhere
// on the way (the path was resolved when recorded: a link there now is a
// redirect), something other than a socket at the end, a missing socket, and
// a path that is not clean and absolute.
func TestDialRefusesWhatWasNotRecorded(t *testing.T) {
	dir := shortDir(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	echoSocket(t, filepath.Join(real, "s"))
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "s"), filepath.Join(dir, "s-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       error
		msg        string
	}{
		{"a linked directory", filepath.Join(dir, "link", "s"), ErrLink, filepath.Join(dir, "link")},
		{"a linked socket", filepath.Join(dir, "s-link"), ErrLink, "s-link"},
		{"a file", filepath.Join(dir, "file"), ErrNotSocket, "a regular file"},
		{"a directory", real, ErrNotSocket, "a directory"},
		{"missing", filepath.Join(dir, "none"), os.ErrNotExist, "none"},
		{"relative", "real/s", nil, "not a clean absolute path"},
		{"unclean", real + "/../real/s", nil, "not a clean absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Dial(context.Background(), tc.path)
			if err == nil {
				c.Close()
				t.Fatal("dialled")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.msg) || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("err %q should name %q and the path", err, tc.msg)
			}
		})
	}
}

// TestResolveRecordsTheRealPath: a source bound through a symbolic link is
// recorded as the path the link leads to, which Dial then reaches.
func TestResolveRecordsTheRealPath(t *testing.T) {
	dir := shortDir(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	echoSocket(t, filepath.Join(real, "s"))
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(filepath.Join(dir, "link", "s"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(real, "s") {
		t.Fatalf("resolved %q", got)
	}
	c, err := Dial(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if out := talkOver(t, c, "x"); out != "echo:x" {
		t.Fatalf("got %q", out)
	}
}

// TestSpliceCarriesHalfCloses: each side's end of input reaches the other as
// a half-close, and an answer sent after it still arrives.
func TestSpliceCarriesHalfCloses(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "late")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		in, _ := io.ReadAll(c)
		time.Sleep(100 * time.Millisecond)
		_, _ = c.Write([]byte("late:" + string(in)))
	}()
	up, err := Dial(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	a, b := unixPair(t)
	done := make(chan struct{})
	go func() {
		Splice(b, up)
		close(done)
	}()
	if got := talkOver(t, a, "req"); got != "late:req" {
		t.Fatalf("got %q", got)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the splice did not end once both sides did")
	}
}

// unixPair is two connected unix stream connections.
func unixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	dir := shortDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "p"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	a, err := net.Dial("unix", filepath.Join(dir, "p"))
	if err != nil {
		t.Fatal(err)
	}
	b := <-acc
	if b == nil {
		t.Fatal("no accepted connection")
	}
	return a, b
}

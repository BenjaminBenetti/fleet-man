//go:build !nok8s

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// echoSocket serves a unix socket at path that answers each connection with
// name: and what it read.
func echoSocket(t *testing.T, path, name string) net.Listener {
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
				_, _ = c.Write([]byte(name + ":" + string(in)))
			}()
		}
	}()
	return ln
}

// socketDirTarget is podTarget binding the daemon-host socket directory dir
// at /tmp/.X11-unix, with X9 shadowed by another bind, and a socket bind of
// its own at /run/gpg.sock.
func socketDirTarget(dir string) kubernetes.PodTarget {
	t := podTarget
	t.HostSockets = []hostrelay.Socket{{Target: "/run/gpg.sock", Source: filepath.Join(dir, "gpg"), Mode: 0o600}}
	t.HostSocketDirs = []hostrelay.SocketDir{{Target: "/tmp/.X11-unix", Source: dir, Shadowed: []string{"X9"}}}
	return t
}

// TestPodSocketDirsFollowTheHost: a pod binds a socket for each socket in
// its socket directories, as listed now: one made there after the create
// comes, one removed goes (the hub's tick tells of each change, and only of
// a change), and neither a shadowed name nor a file that is no socket is
// bound.
func TestPodSocketDirsFollowTheHost(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv("FLEET_SSH_AGENT_SOCK", "off")
	dir, err := hostrelay.Resolve(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	h := newService().podAgents
	target := socketDirTarget(dir)
	live := func(kubernetes.PodTarget) bool { return true }
	echoSocket(t, filepath.Join(dir, "X0"), "X0")
	echoSocket(t, filepath.Join(dir, "X9"), "X9")
	if err := os.WriteFile(filepath.Join(dir, "notes"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := func() []string {
		var out []string
		for _, s := range h.sockets(target).Host {
			out = append(out, s.Target)
		}
		return out
	}
	if got, want := hosts(), []string{"/run/gpg.sock", "/tmp/.X11-unix/X0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host sockets %q, want %q", got, want)
	}
	if h.dirs.tick(live) {
		t.Fatal("a tick with nothing changed tells of a change")
	}
	ln := echoSocket(t, filepath.Join(dir, "X1"), "X1")
	if !h.dirs.tick(live) {
		t.Fatal("a socket made in the directory: no change told")
	}
	if got, want := hosts(), []string{"/run/gpg.sock", "/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after a socket came: %q, want %q", got, want)
	}
	if err := (podwire.DesiredState{Sockets: h.sockets(target)}).Validate(); err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	_ = os.Remove(filepath.Join(dir, "X1"))
	if !h.dirs.tick(live) {
		t.Fatal("a socket removed from the directory: no change told")
	}
	if got, want := hosts(), []string{"/run/gpg.sock", "/tmp/.X11-unix/X0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after a socket went: %q, want %q", got, want)
	}
}

// TestPodSocketDirRelay: a connection to a socket of a bound socket
// directory, one made after the create included, reaches the daemon-host
// socket of that name in the recorded directory; a shadowed name, a name in
// another directory and a file that is no socket are refused.
func TestPodSocketDirRelay(t *testing.T) {
	isolateFleetDir(t)
	dir, err := hostrelay.Resolve(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	h := newService().podAgents
	fg := &fakeGateway{}
	fg.install(h.gateway)
	target := socketDirTarget(dir)
	sess := relayPair(t, func(st *podwire.Stream) { h.inbound(target, st) })
	talk := func(to string) (string, error) {
		conn, err := podwire.DialSock(sess, podwire.TagSockHost, podwire.SockOpen{Target: to})
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = conn.Write([]byte("hello"))
		halfClose(t, conn)
		out, err := io.ReadAll(conn)
		return string(out), err
	}
	echoSocket(t, filepath.Join(dir, "X3"), "X3")
	if out, err := talk("/tmp/.X11-unix/X3"); err != nil || out != "X3:hello" {
		t.Fatalf("a socket made after the create: %q, %v", out, err)
	}
	echoSocket(t, filepath.Join(dir, "X9"), "X9")
	if _, err := talk("/tmp/.X11-unix/X9"); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("a shadowed name: %v", err)
	}
	if _, err := talk("/tmp/other/X3"); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("a name in another directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := talk("/tmp/.X11-unix/plain"); !podwire.IsSockRefused(err, podwire.RefusedUnreachable) {
		t.Fatalf("a file that is no socket: %v", err)
	}
}

// TestPodSocketDirsKeepWithinTheCap: a socket directory with more sockets
// than a pod relays gets the first ones, by name, and fleet.log one warning
// until that changes; a directory that cannot be listed relays none and
// warns once.
func TestPodSocketDirsKeepWithinTheCap(t *testing.T) {
	d := newPodSocketDirs()
	var mu sync.Mutex
	var warnings []string
	d.logWarn = func(msg string, args ...any) {
		mu.Lock()
		warnings = append(warnings, fmt.Sprint(append([]any{msg}, args...)...))
		mu.Unlock()
	}
	var socks []hostrelay.DirSocket
	for i := 0; i < podwire.MaxHostSockets+5; i++ {
		socks = append(socks, hostrelay.DirSocket{Name: fmt.Sprintf("s%02d", i), Mode: 0o600})
	}
	d.list = func(dir string) ([]hostrelay.DirSocket, error) {
		if dir == "/gone" {
			return nil, os.ErrNotExist
		}
		return socks, nil
	}
	target := podTarget
	target.HostSocketDirs = []hostrelay.SocketDir{{Target: "/run/many", Source: "/many"}, {Target: "/run/gone", Source: "/gone"}}
	own := []podwire.HostSocket{{Target: "/run/gpg.sock"}}
	for range 3 {
		got := d.socketsFor(target, own)
		if len(got)+len(own) != podwire.MaxHostSockets || got[0].Target != "/run/many/s00" {
			t.Fatalf("%d sockets (first %+v), want %d with the bind's own", len(got), got[0], podwire.MaxHostSockets)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "more sockets than fleet relays") || !strings.Contains(strings.Join(warnings, "\n"), "cannot list") {
		t.Fatalf("warnings %q, want one for the cap and one for the directory it cannot list", warnings)
	}
}

// TestPodAgentHubTellsThePodsOfSocketDirChanges: the hub's loop has the
// pods told when a socket comes to a directory a pod with a session binds,
// and only then.
func TestPodAgentHubTellsThePodsOfSocketDirChanges(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv("FLEET_SSH_AGENT_SOCK", "off")
	dir, err := hostrelay.Resolve(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	h := newService().podAgents
	fp := &fakePodPool{}
	h.pool = func() podAgentPool { return fp }
	h.interval = 10 * time.Millisecond
	h.gateway.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the pool hooked", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return fp.source != nil
	})
	fp.mu.Lock()
	source := fp.source
	fp.mu.Unlock()
	if got := source(socketDirTarget(dir)); len(got.Host) != 1 {
		t.Fatalf("host sockets %+v, want the bind's own", got.Host)
	}
	time.Sleep(60 * time.Millisecond)
	before := fp.count()
	echoSocket(t, filepath.Join(dir, "X2"), "X2")
	waitFor(t, "a refresh for the new socket", func() bool { return fp.count() > before })
	n := fp.count()
	time.Sleep(60 * time.Millisecond)
	if got := fp.count(); got != n {
		t.Fatalf("refreshes went on with nothing changed: %d, then %d", n, got)
	}
}

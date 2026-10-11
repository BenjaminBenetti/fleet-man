package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// instanceServer is a stand-in for a service in an instance: a local server
// the publisher's dial reaches (the dial counts its calls).
type instanceServer struct {
	addr  string
	dials atomic.Int32
}

func newInstanceServer(t *testing.T, serve func(net.Conn)) *instanceServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
				serve(c)
			}()
		}
	}()
	return &instanceServer{addr: ln.Addr().String()}
}

// dial is a portDial into the stand-in, whatever the port.
func (s *instanceServer) dial(ctx context.Context, _ int) (io.ReadWriteCloser, error) {
	s.dials.Add(1)
	var d net.Dialer
	return d.DialContext(ctx, "tcp", s.addr)
}

// countReply answers how many bytes it read once its client half-closes.
func countReply(c net.Conn) {
	n, _ := io.Copy(io.Discard, c)
	fmt.Fprintf(c, "%d\n", n)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// roundTrip connects to the published port, sends payload, half-closes and
// reads the answer.
func roundTrip(t *testing.T, addr, payload string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

func publishedAddr(p *fleetgrpc.KubePublishedPort) string {
	return net.JoinHostPort(p.GetAddress(), strconv.Itoa(int(p.GetHostPort())))
}

// A published port listens on 127.0.0.1 when no address is named, relays a
// connection into the instance with its half-close, and keeps the port it
// picked for a port left to the daemon.
func TestPublishedPortsServeThroughTheInstanceDial(t *testing.T) {
	srv := newInstanceServer(t, countReply)
	pp := newPortPublisher()
	defer pp.close()
	fixed := freePort(t)
	ports := []backend.PublishedPort{{HostPort: fixed, ContainerPort: 3000}, {HostIP: "127.0.0.1", ContainerPort: 4000}}
	st := pp.sync("c1/web", "alpha/web", "id1", ports, srv.dial)
	if len(st) != 2 || st[0].GetError() != "" || st[1].GetError() != "" {
		t.Fatalf("status %v", st)
	}
	if st[0].GetAddress() != "127.0.0.1" || st[0].GetHostPort() != int32(fixed) || st[0].GetContainerPort() != 3000 {
		t.Fatalf("the named port: %v", st[0])
	}
	picked := st[1].GetHostPort()
	if picked == 0 {
		t.Fatalf("a port left to the daemon should get one: %v", st[1])
	}
	if got := roundTrip(t, publishedAddr(st[0]), "hello there"); got != "11" {
		t.Fatalf("through the published port: %q", got)
	}
	if got := roundTrip(t, publishedAddr(st[1]), "abc"); got != "3" {
		t.Fatalf("through the picked port: %q", got)
	}
	again := pp.sync("c1/web", "alpha/web", "id1", ports, srv.dial)
	if again[1].GetHostPort() != picked {
		t.Fatalf("a later pass should keep the picked port: %d then %d", picked, again[1].GetHostPort())
	}
	if srv.dials.Load() != 2 {
		t.Fatalf("one dial per connection: %d", srv.dials.Load())
	}
}

// A port another process holds is reported (and logged once), and bound on
// a pass after it is free and the retry time has passed.
func TestPublishedPortsReportAConflictAndRetry(t *testing.T) {
	srv := newInstanceServer(t, countReply)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := busy.Addr().(*net.TCPAddr).Port
	pp := newPortPublisher()
	defer pp.close()
	now := time.Now()
	pp.now = func() time.Time { return now }
	ports := []backend.PublishedPort{{HostIP: "127.0.0.1", HostPort: port, ContainerPort: 80}}
	st := pp.sync("k", "alpha/web", "id1", ports, srv.dial)
	if len(st) != 1 || !strings.Contains(st[0].GetError(), "in use") {
		t.Fatalf("a held port should be reported: %v", st)
	}
	_ = busy.Close()
	if st := pp.sync("k", "alpha/web", "id1", ports, srv.dial); st[0].GetError() == "" {
		t.Fatalf("not retried before the retry time: %v", st)
	}
	now = now.Add(publishRetry)
	st = pp.sync("k", "alpha/web", "id1", ports, srv.dial)
	if st[0].GetError() != "" {
		t.Fatalf("a freed port should be bound on the retry: %v", st)
	}
	if got := roundTrip(t, publishedAddr(st[0]), "xy"); got != "2" {
		t.Fatalf("%q", got)
	}
}

// Dropping an instance's ports (it stopped) closes the listeners and the
// connections through them; a rebuilt container starts them over; a port no
// longer published closes.
func TestPublishedPortsFollowTheInstance(t *testing.T) {
	hold := make(chan struct{})
	srv := newInstanceServer(t, func(c net.Conn) { <-hold })
	defer close(hold)
	pp := newPortPublisher()
	defer pp.close()
	a, b := freePort(t), freePort(t)
	both := []backend.PublishedPort{{HostPort: a, ContainerPort: 1}, {HostPort: b, ContainerPort: 2}}
	pp.sync("k", "alpha/web", "id1", both, srv.dial)
	live, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(a)))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	waitUntil(t, "the connection to reach the instance", func() bool { return srv.dials.Load() == 1 })

	// The second port is no longer published.
	pp.sync("k", "alpha/web", "id1", both[:1], srv.dial)
	if listening(b) {
		t.Fatal("a port no longer published still listens")
	}

	pp.drop("k")
	if listening(a) {
		t.Fatal("a stopped instance's port still listens")
	}
	_ = live.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := live.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection through a dropped port should end")
	}

	// A rebuild (another container) listens again, fresh.
	pp.sync("k", "alpha/web", "id1", both[:1], srv.dial)
	pp.sync("k", "alpha/web", "id2", both[:1], srv.dial)
	if !listening(a) {
		t.Fatal("the rebuilt instance's port should listen")
	}

	pp.close()
	if listening(a) || pp.sync("k", "alpha/web", "id2", both[:1], srv.dial) != nil {
		t.Fatal("a closed publisher serves nothing")
	}
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

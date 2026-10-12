//go:build linux

package podagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// loopbackServer serves each connection on 127.0.0.1 with serve and returns
// its port.
func loopbackServer(t *testing.T, serve func(net.Conn)) int {
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
	return ln.Addr().(*net.TCPAddr).Port
}

// TestForwardKeepsATrueHalfClose: the peer reads fleetd's EOF and answers
// seconds later, and the answer still arrives (the runtime's port-forward
// would have closed the connection by then).
func TestForwardKeepsATrueHalfClose(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	port := loopbackServer(t, func(c net.Conn) {
		n, _ := io.Copy(io.Discard, c)
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprintf(c, "late %d\n", n)
	})
	conn, err := s.c.Forward(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("p"), 3<<20)
	go func() {
		_, _ = conn.Write(payload)
		_ = conn.(interface{ CloseWrite() error }).CloseWrite()
	}()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil || strings.TrimSpace(string(got)) != "late "+strconv.Itoa(len(payload)) {
		t.Fatalf("a late answer after the half-close: %q, %v", got, err)
	}
}

func TestForwardRefusesAPortNothingListensOn(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	_, err = s.c.Forward(context.Background(), port)
	if !errors.Is(err, podwire.ErrForwardRefused) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a closed port: %v", err)
	}
	if _, err := s.c.Forward(context.Background(), 0); err == nil {
		t.Fatal("port 0 should be refused before anything is opened")
	}
	// The session is fine: another forward and a command still work.
	echo := loopbackServer(t, func(c net.Conn) { _, _ = io.Copy(c, c) })
	conn, err := s.c.Forward(context.Background(), echo)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("hi"))
	buf := make([]byte, 2)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("echo after a refusal: %q, %v", buf, err)
	}
	_ = conn.Close()
}

// TestForwardCarriesManyConnections: connections at once each carry their own
// bytes, and the peer's close ends each read with EOF.
func TestForwardCarriesManyConnections(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	echo := loopbackServer(t, func(c net.Conn) { _, _ = io.Copy(c, c) })
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.c.Forward(context.Background(), echo)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte{byte('a' + i)}, 256<<10)
			go func() {
				_, _ = conn.Write(payload)
				_ = conn.(interface{ CloseWrite() error }).CloseWrite()
			}()
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			got, err := io.ReadAll(conn)
			if err != nil || !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("connection %d: %d bytes back, %v", i, len(got), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestForwardCloseEndsAPeerThatKeepsSending: fleetd's Close of a forward
// whose pod peer keeps sending (a server-sent event stream) ends the agent's
// side at once (StreamAbort): the agent's FIN comes within seconds, so the
// stream's slot is free (not after the 30s drain, with the agent's side left
// to yamux's close timeout minutes later), and the peer sees its connection
// close. A half-close keeps it open.
func TestForwardCloseEndsAPeerThatKeepsSending(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	peerDone := make(chan struct{}, 4)
	port := loopbackServer(t, func(c net.Conn) {
		defer func() { peerDone <- struct{}{} }()
		for {
			if _, err := c.Write([]byte("data: tick\n\n")); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	active := s.c.Active

	half, err := s.c.Forward(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	_ = half.(interface{ CloseWrite() error }).CloseWrite()
	buf := make([]byte, 64)
	_ = half.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAtLeast(half, buf, 1); err != nil {
		t.Fatalf("a half-closed forward stopped carrying the peer's events: %v", err)
	}

	conn, err := s.c.Forward(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAtLeast(conn, buf, 1); err != nil {
		t.Fatal(err)
	}
	if n := active(); n != 2 {
		t.Fatalf("%d forward streams, want 2", n)
	}
	_ = conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	for active() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the closed forward still holds its stream: %d open", active())
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-peerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the pod's peer was not disconnected")
	}
	_ = half.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAtLeast(half, buf, 1); err != nil {
		t.Fatalf("the other forward was ended too: %v", err)
	}
	_ = half.Close()
}

// TestForwardAbortRightAfterItsOpen: a StreamAbort fleetd sends as soon as
// the OPEN reply reaches it (a connection closed at once) still ends the
// agent's side and the pod's peer: the agent tracks the stream before it
// replies, so the abort cannot come before the agent knows the stream.
func TestForwardAbortRightAfterItsOpen(t *testing.T) {
	// Cleared once the agent is gone (cleanups run last first).
	t.Cleanup(func() { forwardOpened = nil })
	forwardOpened = func() { time.Sleep(300 * time.Millisecond) }
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	peerDone := make(chan struct{}, 1)
	port := loopbackServer(t, func(c net.Conn) {
		defer func() { peerDone <- struct{}{} }()
		for {
			if _, err := c.Write([]byte("data: tick\n\n")); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	conn, err := s.c.Forward(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case <-peerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("a forward closed right after its open still relays the pod's peer: its StreamAbort was lost")
	}
}

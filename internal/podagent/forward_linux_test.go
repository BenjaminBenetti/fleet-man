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

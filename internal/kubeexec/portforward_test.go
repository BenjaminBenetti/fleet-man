//go:build !nok8s

package kubeexec

import (
	"bufio"
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

	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec/fakekubelet"
)

// countServer replies with how many bytes it read once its client
// half-closes, after delay.
func countServer(delay time.Duration) func(net.Conn) {
	return func(c net.Conn) {
		n, _ := io.Copy(io.Discard, c)
		time.Sleep(delay)
		fmt.Fprintf(c, "%d\n", n)
	}
}

func echoServer(c net.Conn) { _, _ = io.Copy(c, c) }

func listen(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	addr, stop, err := fakekubelet.Listen(serve)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return addr
}

func newKubelet(t *testing.T) *fakekubelet.Kubelet {
	t.Helper()
	k := fakekubelet.New()
	t.Cleanup(k.Close)
	return k
}

func dialPF(t *testing.T, k *fakekubelet.Kubelet) *PortForward {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pf, err := DialPortForward(ctx, k.Config(), "ns", "pod-0")
	if err != nil {
		t.Fatalf("DialPortForward: %v", err)
	}
	t.Cleanup(func() { _ = pf.Close() })
	return pf
}

// halfCloseExchange writes payload, half-closes and reads everything back.
func halfCloseExchange(t *testing.T, c *PortConn, payload []byte) (string, error) {
	t.Helper()
	go func() {
		_, _ = c.Write(payload)
		_ = c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	return string(got), err
}

func TestPortForwardCarriesDataAndHalfCloses(t *testing.T) {
	k := newKubelet(t)
	k.Route(80, listen(t, countServer(0)))
	k.Route(81, listen(t, echoServer))
	pf := dialPF(t, k)

	c, err := pf.Dial(80)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 1<<20)
	got, err := halfCloseExchange(t, c, payload)
	_ = c.Close()
	if err != nil || strings.TrimSpace(got) != strconv.Itoa(len(payload)) {
		t.Fatalf("after a half-close the peer should count %d bytes and answer: got %q, %v", len(payload), got, err)
	}

	c, err = pf.Dial(81)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo: %q, %v", line, err)
	}
	if k.Upgrades() != 1 {
		t.Fatalf("both connections should share one upgrade, got %d", k.Upgrades())
	}
}

func TestPortForwardRefusedPortFailsOnlyItsConnection(t *testing.T) {
	k := newKubelet(t)
	k.Route(80, listen(t, echoServer))
	pf := dialPF(t, k)

	refused, err := pf.Dial(9)
	if err != nil {
		t.Fatalf("the dial itself succeeds; the kubelet answers on the error stream: %v", err)
	}
	_, err = io.ReadAll(refused)
	_ = refused.Close()
	var perr *PortError
	if !errors.As(err, &perr) || perr.Port != 9 || !strings.Contains(perr.Message, "connection refused") {
		t.Fatalf("a refused port should read as a *PortError naming it: %v", err)
	}

	c, err := pf.Dial(80)
	if err != nil {
		t.Fatalf("the connection should survive a refused port: %v", err)
	}
	defer c.Close()
	got, err := halfCloseExchange(t, c, []byte("still here"))
	if err != nil || got != "still here" {
		t.Fatalf("after the refusal: %q, %v", got, err)
	}
	if !pf.Alive() || k.Upgrades() != 1 {
		t.Fatalf("one connection, still alive: alive=%v upgrades=%d", pf.Alive(), k.Upgrades())
	}
}

func TestPortForwardCarriesFiftyConnectionsAtOnce(t *testing.T) {
	k := newKubelet(t)
	k.Route(80, listen(t, echoServer))
	pf := dialPF(t, k)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := pf.Dial(80)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			payload := bytes.Repeat([]byte{byte('a' + i%26)}, 64<<10)
			go func() {
				_, _ = c.Write(payload)
				_ = c.CloseWrite()
			}()
			got, err := io.ReadAll(c)
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
	if k.Upgrades() != 1 || k.Streams() != n {
		t.Fatalf("%d connections should be %d stream pairs on one upgrade: upgrades=%d streams=%d", n, n, k.Upgrades(), k.Streams())
	}
	deadline := time.Now().Add(5 * time.Second)
	for pf.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pf.Active() != 0 || pf.IdleSince().IsZero() {
		t.Fatalf("every connection closed: active=%d idle since %v", pf.Active(), pf.IdleSince())
	}
}

func TestPortForwardForbidden(t *testing.T) {
	k := newKubelet(t)
	k.Forbid(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := DialPortForward(ctx, k.Config(), "ns", "pod-0")
	if !errors.Is(err, ErrPortForwardForbidden) || !IsForbidden(err) {
		t.Fatalf("a 403 should read as ErrPortForwardForbidden: %v", err)
	}
	if !strings.Contains(err.Error(), "pods/portforward") {
		t.Fatalf("the error should keep the API server's words: %v", err)
	}
}

func TestPortForwardEndsWhenTheServerDropsIt(t *testing.T) {
	k := newKubelet(t)
	k.Route(80, listen(t, echoServer))
	pf := dialPF(t, k)
	// The upgrade lands on the client before the server has counted it.
	for deadline := time.Now().Add(5 * time.Second); k.Open() != 1; {
		if time.Now().After(deadline) {
			t.Fatal("the server never saw the connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	k.Drop()
	select {
	case <-pf.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done never closed after the server dropped the connection")
	}
	if _, err := pf.Dial(80); !errors.Is(err, ErrPortForwardClosed) {
		t.Fatalf("a dial on a dropped connection: %v", err)
	}
}

func TestPortForwardDialHonoursItsContext(t *testing.T) {
	// Nothing answers the upgrade: a listener that never replies.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	cfg := newKubelet(t).Config()
	cfg.Host = "http://" + ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = DialPortForward(ctx, cfg, "ns", "pod-0")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("the dial should end with its context: %v after %v", err, time.Since(start))
	}
}

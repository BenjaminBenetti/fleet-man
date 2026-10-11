//go:build !nok8s

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec/fakekubelet"
)

// The shim's forwards against a fake kubelet, with fleetd away (no socket):
// the shim loads the kubeconfig it was given, as when fleetd is down.
func forwardShimFlags(t *testing.T, k *fakekubelet.Kubelet) kubeExecFlags {
	t.Helper()
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: %q}
contexts:
- name: ctx
  context: {cluster: c, user: u}
users:
- name: u
  user: {token: tok}
`, k.Config().Host)
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return kubeExecFlags{socket: filepath.Join(dir, "no-fleetd.sock"), cluster: "c1", context: "ctx", kubeconfig: []string{kc}, namespace: "ns", pod: "pod-0"}
}

func shimServer(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	addr, stop, err := fakekubelet.Listen(serve)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return addr
}

// swapStdio points os.Stdin and os.Stdout at pipes for the test.
func swapStdio(t *testing.T) (stdin *os.File, stdout *os.File) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() {
		os.Stdin, os.Stdout = origIn, origOut
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	return inW, outR
}

// forward-stdio: stdin to the pod's port, the end of stdin a half-close, the
// peer's answer to stdout.
func TestKubeExecForwardStdio(t *testing.T) {
	k := fakekubelet.New()
	defer k.Close()
	k.Route(5000, shimServer(t, func(c net.Conn) {
		n, _ := io.Copy(io.Discard, c)
		time.Sleep(100 * time.Millisecond)
		fmt.Fprintf(c, "counted %d\n", n)
	}))
	f := forwardShimFlags(t, k)
	stdin, stdout := swapStdio(t)
	done := make(chan error, 1)
	go func() { done <- runKubeExecForwardStdio(context.Background(), f, 5000) }()
	_, _ = io.WriteString(stdin, "twelve bytes")
	_ = stdin.Close()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "counted 12\n" {
		t.Fatalf("through forward-stdio: %q, %v", line, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("forward-stdio did not end with the connection")
	}

	// A port nothing listens on is the kubelet's error.
	_, _ = swapStdio(t)
	if err := runKubeExecForwardStdio(context.Background(), f, 5001); err == nil || !strings.Contains(err.Error(), "port 5001") {
		t.Fatalf("a refused port: %v", err)
	}
}

// port-forward: connections to 127.0.0.1:<local> share one port-forward
// connection to the pod.
func TestKubeExecPortForward(t *testing.T) {
	k := fakekubelet.New()
	defer k.Close()
	k.Route(80, shimServer(t, func(c net.Conn) { _, _ = io.Copy(c, c) }))
	f := forwardShimFlags(t, k)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	local := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runKubeExecPortForward(ctx, f, local, 80) }()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(local))
	for i := range 3 {
		var c net.Conn
		var err error
		deadline := time.Now().Add(5 * time.Second)
		for c, err = net.Dial("tcp", addr); err != nil && time.Now().Before(deadline); c, err = net.Dial("tcp", addr) {
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		msg := fmt.Sprintf("ping %d", i)
		_, _ = io.WriteString(c, msg)
		_ = c.(*net.TCPConn).CloseWrite()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got, err := io.ReadAll(c)
		_ = c.Close()
		if err != nil || string(got) != msg {
			t.Fatalf("connection %d: %q, %v", i, got, err)
		}
	}
	if k.Upgrades() != 1 {
		t.Fatalf("the connections should share one port-forward: %d upgrades", k.Upgrades())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("port-forward did not stop with its context")
	}
}

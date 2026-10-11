//go:build !nok8s

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// runKubeExecForwardStdio connects stdin and stdout to port in the pod: the
// end of stdin half-closes the connection, and it ends when the pod's peer
// closes it (or refuses it: the kubelet's answer is the error).
func runKubeExecForwardStdio(ctx context.Context, f kubeExecFlags, port int, stdin io.Reader, stdout io.Writer) error {
	if f.cluster == "" || f.namespace == "" || f.pod == "" {
		return errNoPod
	}
	t, _, err := kubeExecTransport(ctx, f, f.pod, false)
	if err != nil {
		return err
	}
	pf, err := t.PortForward(ctx, f.namespace, f.pod)
	if err != nil {
		return err
	}
	defer pf.Close()
	conn, err := pf.Dial(port)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, stdin)
		_ = conn.CloseWrite()
	}()
	_, err = io.Copy(stdout, conn)
	return err
}

// runKubeExecPortForward listens on 127.0.0.1:local until interrupted and
// forwards each connection to remote in the pod, all of them stream pairs
// on one port-forward connection (a new one once it drops).
func runKubeExecPortForward(ctx context.Context, f kubeExecFlags, local, remote int) error {
	if f.cluster == "" || f.namespace == "" || f.pod == "" {
		return errNoPod
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	var mu sync.Mutex
	var pf *kubeexec.PortForward
	defer func() {
		mu.Lock()
		if pf != nil {
			_ = pf.Close()
		}
		mu.Unlock()
	}()
	connect := func() (*kubeexec.PortConn, error) {
		mu.Lock()
		defer mu.Unlock()
		if pf == nil || !pf.Alive() {
			t, _, err := kubeExecTransport(ctx, f, f.pod, pf != nil)
			if err != nil {
				return nil, err
			}
			if pf, err = t.PortForward(ctx, f.namespace, f.pod); err != nil {
				return nil, err
			}
		}
		return pf.Dial(remote)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer c.Close()
			conn, err := connect()
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet kube-exec port-forward: %v\n", err)
				return
			}
			defer conn.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = io.Copy(c, conn)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
			_, _ = io.Copy(conn, c)
			_ = conn.CloseWrite()
			<-done
		}()
	}
}

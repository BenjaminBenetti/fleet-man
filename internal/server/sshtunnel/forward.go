package sshtunnel

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// forward.go runs the long-lived `ssh -N -L` port-forward for one remote and
// waits for it to become usable.

// forwardReadyTimeout bounds the wait for ssh to bind the local forward (connect
// + auth; ssh only binds -L listeners once authenticated).
const forwardReadyTimeout = 30 * time.Second

// forwardProc is a running forward: Done closes when the process exits, Err is
// its captured stderr (last line) once done, Kill tears it down.
type forwardProc interface {
	Done() <-chan struct{}
	Err() string
	Kill()
}

// sshForward is the production forwardProc: an `ssh -N -L` child.
type sshForward struct {
	cmd    *exec.Cmd
	stderr *boundedBuffer
	done   chan struct{}
	once   sync.Once
}

func (f *sshForward) Done() <-chan struct{} { return f.done }
func (f *sshForward) Err() string           { return lastLine(f.stderr.String()) }
func (f *sshForward) Kill() {
	f.once.Do(func() {
		if f.cmd.Process != nil {
			_ = f.cmd.Process.Kill()
		}
	})
}

// startForward spawns `ssh -N -L 127.0.0.1:<local>:127.0.0.1:<remote> target`.
// ctx is the daemon lifetime (the child dies with it); ExitOnForwardFailure
// makes a local bind failure (port taken) a prompt non-zero exit rather than a
// silently useless session, which the Manager retries on a fresh port.
func startForward(ctx context.Context, t Target, localPort, remotePort int) (forwardProc, error) {
	spec := fmt.Sprintf("%s:%s", loopback(localPort), loopback(remotePort))
	cmd := exec.CommandContext(ctx, "ssh", t.sshArgs("-o", "ExitOnForwardFailure=yes", "-N", "-L", spec)...)
	stderr := &boundedBuffer{max: 4096}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	setSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	f := &sshForward{cmd: cmd, stderr: stderr, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(f.done)
	}()
	return f, nil
}

// waitForwardReady polls the local forward port until ssh accepts on it (bound
// after auth), the process exits (its stderr becomes the error), or the timeout.
// The exit check comes FIRST each round so a prompt ssh failure (auth, host
// key, bind) is reported as such rather than masked by a probe.
func waitForwardReady(ctx context.Context, proc forwardProc, localPort int) error {
	deadline := time.After(forwardReadyTimeout)
	addr := loopback(localPort)
	for {
		select {
		case <-proc.Done():
			if msg := proc.Err(); msg != "" {
				return fmt.Errorf("ssh: %s", msg)
			}
			return fmt.Errorf("ssh exited before the tunnel came up")
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-proc.Done():
			if msg := proc.Err(); msg != "" {
				return fmt.Errorf("ssh: %s", msg)
			}
			return fmt.Errorf("ssh exited before the tunnel came up")
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("ssh tunnel did not come up within %s", forwardReadyTimeout)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// portFree reports whether the loopback port can be bound right now (the
// pre-spawn check that keeps a squatter from being mistaken for our forward).
func portFree(port int) bool {
	l, err := net.Listen("tcp", loopback(port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// isBindFailure recognises ssh's "local port already in use" diagnostics, the
// one forward failure worth retrying on another port.
func isBindFailure(msg string) bool {
	return strings.Contains(msg, "Address already in use") || strings.Contains(msg, "Could not request local forwarding")
}

// freePort asks the kernel for an unused loopback port (bind :0, read, close).
// The port can in principle be taken before ssh binds it — the Manager retries
// a bind failure with a new one.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// boundedBuffer keeps the LAST max bytes written (ssh's diagnostics are short,
// but a verbose session must not grow without bound over a long tunnel life).
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if b.buf.Len() > b.max {
		trimmed := b.buf.Bytes()[b.buf.Len()-b.max:]
		b.buf = *bytes.NewBuffer(append([]byte(nil), trimmed...))
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

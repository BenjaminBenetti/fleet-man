package server

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// execStreamConn is a backend.ExecStream as the io.ReadWriteCloser the mic
// and audio output hubs drive, the shape startStdioBridge gives a host
// command: Write feeds the command's stdin, Read yields its stdout, CloseWrite
// half-closes stdin, and Close aborts the command (and reaps it). Stderr is
// drained and dropped, as the host bridge drops a command's stderr.
type execStreamConn struct {
	stream    backend.ExecStream
	closeOnce sync.Once
}

// openExecStreamConn starts req through execer and wraps it. timeout bounds
// the start.
func openExecStreamConn(execer backend.StreamExecer, req backend.ExecRequest, timeout time.Duration) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stream, err := execer.OpenExec(ctx, req)
	if err != nil {
		return nil, err
	}
	go func() { _, _ = io.Copy(io.Discard, stream.Stderr()) }()
	return &execStreamConn{stream: stream}, nil
}

func (c *execStreamConn) Read(p []byte) (int, error)  { return c.stream.Stdout().Read(p) }
func (c *execStreamConn) Write(p []byte) (int, error) { return c.stream.Stdin().Write(p) }
func (c *execStreamConn) CloseWrite() error           { return c.stream.Stdin().Close() }

func (c *execStreamConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stream.Close()
		// Reap off the closing goroutine, as stdioBridge does.
		go func() { _, _ = c.stream.Wait() }()
	})
	return nil
}

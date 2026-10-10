package kubeshim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// requestTimeout bounds how long a new connection may take to send its
// request.
const requestTimeout = 10 * time.Second

// ServeExec serves pod-mode execs on ln with ex until ctx is done or ln
// fails, then closes ln, cancels the execs still running and waits for them.
// It returns nil when ctx ended it.
func ServeExec(ctx context.Context, ln net.Listener, ex Execer) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Cancel first, then wait: however the loop ends (ctx, or ln failing),
	// the execs still running are told to end rather than waited out.
	defer func() {
		cancel()
		wg.Wait()
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveExecConn(ctx, conn, ex)
		}()
	}
}

// serveExecConn runs the one exec a connection carries.
func serveExecConn(ctx context.Context, conn net.Conn, ex Execer) {
	defer conn.Close()
	fw := &frameWriter{w: conn}

	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	typ, payload, err := readFrame(conn)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req ExecRequest
	if typ != frameRequest {
		_ = fw.write(frameError, []byte("exec socket: expected a request first"))
		return
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = fw.write(frameError, []byte(fmt.Sprintf("exec socket: bad request: %v", err)))
		return
	}
	if len(req.Argv) == 0 {
		_ = fw.write(frameError, []byte("exec socket: no command"))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The shim going away (a closed connection, a failed write) ends the exec.
	stdout := cancelOnError{w: streamWriter{fw: fw, typ: frameStdout}, cancel: cancel}
	stderr := cancelOnError{w: streamWriter{fw: fw, typ: frameStderr}, cancel: cancel}

	var stdinR *io.PipeReader
	var stdinW *io.PipeWriter
	if req.Stdin {
		stdinR, stdinW = io.Pipe()
		// A cancelled exec must not leave the frame reader blocked handing
		// stdin to a command that is not reading it.
		go func() {
			<-ctx.Done()
			_ = stdinR.Close()
		}()
	}
	resize := make(chan WindowSize, 4)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readShimFrames(conn, stdinW, resize, cancel)
	}()

	stdio := ExecStdio{Stdout: stdout, Stderr: stderr, Resize: resize}
	if stdinR != nil {
		stdio.Stdin = stdinR
	}
	code, err := ex.Exec(ctx, req, stdio)
	if err != nil {
		_ = fw.write(frameError, []byte(err.Error()))
	} else {
		_ = fw.write(frameExit, exitPayload(code))
	}
	// Unblock the frame reader: it may be waiting to hand stdin to a command
	// that no longer reads it, or for the shim's next frame.
	if stdinR != nil {
		_ = stdinR.Close()
	}
	_ = conn.Close()
	<-readerDone
}

// readShimFrames takes the shim's stdin and resizes until it closes the
// connection, which cancels the exec.
func readShimFrames(conn net.Conn, stdin *io.PipeWriter, resize chan<- WindowSize, cancel context.CancelFunc) {
	stdinOpen := stdin != nil
	closeStdin := func(err error) {
		if stdinOpen {
			_ = stdin.CloseWithError(err)
			stdinOpen = false
		}
	}
	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			closeStdin(io.ErrUnexpectedEOF)
			cancel()
			return
		}
		switch typ {
		case frameStdin:
			if stdinOpen {
				if _, err := stdin.Write(payload); err != nil {
					// The command stopped reading; drop the rest.
					stdinOpen = false
				}
			}
		case frameStdinEOF:
			closeStdin(nil)
		case frameResize:
			var ws WindowSize
			if json.Unmarshal(payload, &ws) == nil {
				select {
				case resize <- ws:
				default:
				}
			}
		}
	}
}

// cancelOnError is a writer whose first failure cancels the exec: the shim it
// writes to is gone.
type cancelOnError struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (c cancelOnError) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil {
		c.cancel()
		return n, errors.Join(errShimGone, err)
	}
	return n, nil
}

// errShimGone is a write to a shim that went away.
var errShimGone = errors.New("the docker shim that asked for this exec is gone")

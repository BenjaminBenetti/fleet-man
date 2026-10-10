package kubeshim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
)

// runExecClient sends req down the exec socket and relays the exec: stdin
// (with -i) up, output down, terminal resizes up (with -t on a terminal). It
// returns the command's exit code, or 125 when the exec could not run.
func runExecClient(inv invocation, socket string, req ExecRequest) int {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		fmt.Fprintf(inv.stderr, "kubeshim: exec: connecting to fleetd: %v\n", err)
		return exitShim
	}
	defer conn.Close()
	fw := &frameWriter{w: conn}

	// The server reads the request first, so resizes are forwarded only once
	// it is sent; the terminal is raw before then, as with docker exec -t.
	requestSent := make(chan struct{})
	if req.TTY && inv.isTerminal(inv.stdin) {
		f := inv.stdin.(*os.File)
		if cols, rows, err := term.GetSize(f.Fd()); err == nil && cols > 0 && rows > 0 {
			req.Size = &WindowSize{Rows: uint16(rows), Cols: uint16(cols)}
		}
		defer rawTerminal(f, fw, requestSent)()
	}

	payload, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(inv.stderr, "kubeshim: exec: %v\n", err)
		return exitShim
	}
	if err := fw.write(frameRequest, payload); err != nil {
		fmt.Fprintf(inv.stderr, "kubeshim: exec: sending the request: %v\n", err)
		return exitShim
	}
	close(requestSent)
	if req.Stdin {
		go pumpExecStdin(fw, inv.stdin)
	}

	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("fleetd closed the exec before it ended")
			}
			fmt.Fprintf(inv.stderr, "kubeshim: exec: %v\n", err)
			return exitShim
		}
		switch typ {
		case frameStdout:
			if _, err := inv.stdout.Write(payload); err != nil {
				return exitShim
			}
		case frameStderr:
			if _, err := inv.stderr.Write(payload); err != nil {
				return exitShim
			}
		case frameExit:
			code, err := exitCode(payload)
			if err != nil {
				fmt.Fprintf(inv.stderr, "kubeshim: exec: %v\n", err)
				return exitShim
			}
			return code
		case frameError:
			fmt.Fprintf(inv.stderr, "kubeshim: exec in the pod failed: %s\n", payload)
			return exitShim
		}
	}
}

// pumpExecStdin sends stdin up until it ends, then says so. A failed send
// just stops it: the frame loop reports what became of the connection.
func pumpExecStdin(fw *frameWriter, stdin io.Reader) {
	if _, err := io.Copy(streamWriter{fw: fw, typ: frameStdin}, stdin); err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return
		}
	}
	_ = fw.write(frameStdinEOF, nil)
}

// rawTerminal puts the terminal f in raw mode, as docker exec -t does, and
// forwards its resizes once ready is closed (the request has gone first). The
// returned func restores it; it also runs when a signal ends the shim.
func rawTerminal(f *os.File, fw *frameWriter, ready <-chan struct{}) func() {
	state, rawErr := term.MakeRaw(f.Fd())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	done := make(chan struct{})
	restore := func() {
		if rawErr == nil {
			_ = term.Restore(f.Fd(), state)
		}
	}
	go func() {
		select {
		case <-ready:
		case <-done:
			return
		}
		for {
			select {
			case <-done:
				return
			case s := <-sig:
				if s != syscall.SIGWINCH {
					restore()
					os.Exit(128 + int(s.(syscall.Signal)))
				}
				if cols, rows, err := term.GetSize(f.Fd()); err == nil && cols > 0 && rows > 0 {
					data, _ := json.Marshal(WindowSize{Rows: uint16(rows), Cols: uint16(cols)})
					_ = fw.write(frameResize, data)
				}
			}
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		restore()
	}
}

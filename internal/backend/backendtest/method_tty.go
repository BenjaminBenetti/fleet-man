package backendtest

import (
	"io"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/creack/pty"
)

// ttyScript proves it has a terminal, reports its size, reports it again on
// every line of input, and ends at the terminal's EOF (^D).
const ttyScript = `[ -t 0 ] && [ -t 1 ] && echo is-a-tty; stty size; while read -r line; do stty size; done; echo tty-done`

// ttySession is a command running on a terminal, however the backend runs one.
type ttySession struct {
	in     io.Writer
	resize func(rows, cols uint16) error
	wait   func() (int, error)
	abort  func()

	mu     sync.Mutex
	output strings.Builder
	read   chan struct{} // closed when the output ends
}

func (s *ttySession) drain(out io.Reader) {
	defer close(s.read)
	buf := make([]byte, 4096)
	for {
		n, err := out.Read(buf)
		s.mu.Lock()
		s.output.Write(buf[:n])
		s.mu.Unlock()
		if err != nil {
			return // EOF, or EIO from a PTY whose command exited
		}
	}
}

func (s *ttySession) seen(text string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.output.String()
	return strings.Contains(out, text), out
}

// startTTY runs ttyScript on a 24x80 terminal the way the Exec RPC does: in
// the backend's own stream when it is a StreamExecer, else ExecCommandQuiet on
// a host PTY.
func startTTY(c *C, i *Instance) *ttySession {
	c.Helper()
	argv := ShArgv(ttyScript)
	s := &ttySession{read: make(chan struct{})}
	if execer, ok := i.B.(backend.StreamExecer); ok {
		stream, err := execer.OpenExec(c.Ctx, backend.ExecRequest{
			Class: backend.ExecClassExec, WorkspaceDir: i.WorkspaceDir(), ContainerID: i.ID(), Argv: argv, TTY: true,
		})
		if err != nil {
			c.Fatalf("OpenExec with a TTY: %v", err)
		}
		if err := stream.Resize(24, 80); err != nil {
			c.Fatalf("Resize: %v", err)
		}
		s.in, s.resize, s.wait = stream.Stdin(), stream.Resize, stream.Wait
		s.abort = func() { _ = stream.Close() }
		go s.drain(stream.Stdout())
		return s
	}
	cmd := i.B.ExecCommandQuiet(i.WorkspaceDir(), argv).Cmd
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		c.Fatalf("start on a PTY: %v", err)
	}
	s.in = ptmx
	s.resize = func(rows, cols uint16) error { return pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}) }
	s.wait = func() (int, error) {
		err := cmd.Wait()
		_ = ptmx.Close()
		if code := exitCode(err); code >= 0 {
			return code, nil
		}
		return -1, err
	}
	s.abort = func() { _ = cmd.Process.Kill() }
	go s.drain(ptmx)
	return s
}

// caseExecTTY: a command run with a terminal has one, of the size asked for,
// sees a resize, and ends at the terminal's EOF.
func caseExecTTY(c *C) {
	i := c.Shared()
	s := startTTY(c, i)
	finished := false
	defer func() {
		if !finished {
			s.abort()
			_, _ = s.wait()
		}
	}()
	for _, want := range []string{"is-a-tty", "24 80"} {
		c.Eventually("the command to report "+want, 30*time.Second, func() (bool, string) { return s.seen(want) })
	}
	if err := s.resize(40, 132); err != nil {
		c.Fatalf("resize: %v", err)
	}
	c.Eventually("the command to see the resize to 40x132", 30*time.Second, func() (bool, string) {
		if _, err := s.in.Write([]byte("\n")); err != nil {
			return false, "write: " + err.Error()
		}
		return s.seen("40 132")
	})
	if _, err := s.in.Write([]byte{4}); err != nil { // ^D
		c.Fatalf("send EOF: %v", err)
	}
	select {
	case <-s.read:
	case <-time.After(c.within(30 * time.Second)):
		_, out := s.seen("")
		c.Fatalf("the command did not end at the terminal's EOF; output:\n%s", out)
	}
	finished = true
	code, err := s.wait()
	if ok, out := s.seen("tty-done"); !ok || code != 0 || err != nil {
		c.Errorf("the command ended with code %d (err %v); output:\n%s", code, err, out)
	}
}

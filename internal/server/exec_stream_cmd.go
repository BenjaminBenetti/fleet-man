package server

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/creack/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cmdExecStream is a host command (a backend's `docker exec`) run as a
// backend.ExecStream: how the Exec RPC runs a command for a backend that has
// no StreamExecer of its own.
type cmdExecStream struct {
	cmd *exec.Cmd
	// pty is the PTY's master end, with a TTY; nil without one.
	pty    *os.File
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

// startCmdExecStream starts cmd on a fresh PTY (tty) or with separate
// stdin/stdout/stderr pipes. A failure is the gRPC status the Exec RPC returns.
func startCmdExecStream(cmd *exec.Cmd, tty bool) (backend.ExecStream, error) {
	if tty {
		ptmx, err := pty.Start(cmd)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "start exec (tty): %v", err)
		}
		return &cmdExecStream{cmd: cmd, pty: ptmx, stdin: ptyInput{ptmx}, stdout: ptmx, stderr: strings.NewReader("")}, nil
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, status.Errorf(codes.Unavailable, "start exec: %v", err)
	}
	return &cmdExecStream{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (s *cmdExecStream) Stdin() io.WriteCloser { return s.stdin }
func (s *cmdExecStream) Stdout() io.Reader     { return s.stdout }
func (s *cmdExecStream) Stderr() io.Reader     { return s.stderr }

func (s *cmdExecStream) Resize(rows, cols uint16) error {
	if s.pty == nil {
		return nil
	}
	return pty.Setsize(s.pty, &pty.Winsize{Rows: rows, Cols: cols})
}

// Wait reaps the command, then closes the PTY. A command that ran and failed
// gives its real exit code (-1 if a signal killed it); anything else (a Wait
// plumbing failure) is -1 with the error.
func (s *cmdExecStream) Wait() (int, error) {
	err := s.cmd.Wait()
	if s.pty != nil {
		_ = s.pty.Close()
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// Close kills the command; Wait reaps it. After Wait it does nothing (the
// process is gone).
func (s *cmdExecStream) Close() error {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return nil
}

// ptyInput is a PTY master as a command's stdin. A PTY has no half-close, so
// Close leaves it open (Wait closes it): input ends in-band (^D).
type ptyInput struct{ ptmx *os.File }

func (p ptyInput) Write(b []byte) (int, error) { return p.ptmx.Write(b) }
func (p ptyInput) Close() error                { return nil }

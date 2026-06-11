package server

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/creack/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exec_stream.go is the streaming half of exec (issue #141): one Exec bidi
// stream per command run inside an instance, for REMOTE clients that have no
// local backend and so cannot use the ResolveExecCommand carve-out (exec.go).
// The first client frame must carry ExecStart; subsequent frames carry stdin
// bytes (or TTY resizes). The server runs the backend's exec command, pipes
// stdin/stdout/stderr (or a PTY) over the stream, and finishes with a terminal
// ExecExit frame carrying the command's exit code.

// execChunkSize bounds one output frame. 32 KiB matches forwardChunkSize and
// io.Copy's default buffer; well under gRPC's 4 MB message ceiling.
const execChunkSize = 32 * 1024

// buildExecCommand builds the host-side command that runs argv inside the
// instance's container. Package var so tests can stub the whole backend layer.
// ExecCommandQuiet, not ExecCommand: the wrapper's completion log only fires
// through its Run/Output methods (which this handler bypasses), and Exec logs
// its own single per-session line instead.
var buildExecCommand = func(inst *fleet.Instance, argv []string) *exec.Cmd {
	return backendutil.NewForInstance(inst, false).ExecCommandQuiet(inst.WorkspaceDir, argv).Cmd
}

// Exec implements the remote exec data plane. The first client frame must
// carry start; afterwards frames carry stdin bytes (or resize, TTY only). The
// server replies with stdout/stderr frames and a terminal exit. A client
// CloseSend half-closes stdin (so cat-style commands finish) without killing
// the command; a vanished client (stream context done) kills it.
func (s *service) Exec(stream fleetgrpc.FleetService_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "first Exec frame must carry start")
	}
	argv := start.GetArgv()
	if len(argv) == 0 {
		return status.Error(codes.InvalidArgument, "exec argv must not be empty")
	}
	inst, err := resolveServerInstance(start.GetFleet(), start.GetInstance())
	if err != nil {
		return err
	}

	cmd := buildExecCommand(inst, argv)
	applyExecEnv(cmd, start.GetEnv())

	began := time.Now()
	snd := &execSender{stream: stream}
	var code int32
	var errMsg *string
	if start.GetTty() {
		code, errMsg, err = runExecTTY(stream, snd, cmd)
	} else {
		code, errMsg, err = runExecPipes(stream, snd, cmd)
	}
	// One per-session log line (the ExecCommandQuiet counterpart of
	// flog.ContainerExec), recorded when the session finishes.
	logArgs := []any{
		"fleet", start.GetFleet(), "instance", start.GetInstance(),
		"cmd", strings.Join(argv, " "), "tty", start.GetTty(),
	}
	if err != nil {
		flog.Error("exec session", append(logArgs, "ms", flog.MillisSince(began), "err", err)...)
		return err
	}
	logArgs = append(logArgs, "code", code, "ms", flog.MillisSince(began))
	if errMsg != nil {
		flog.Error("exec session", append(logArgs, "err", *errMsg)...)
	} else {
		flog.Info("exec session", logArgs...)
	}
	return snd.sendExit(code, errMsg)
}

// runExecTTY runs cmd on a fresh PTY and pipes it over the stream. The PTY
// merges stdout and stderr, so all output travels as stdout frames; resize
// frames apply to the PTY (including a resize that arrives before any output —
// the recv pump starts as soon as the command does).
func runExecTTY(stream fleetgrpc.FleetService_ExecServer, snd *execSender, cmd *exec.Cmd) (int32, *string, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 0, nil, status.Errorf(codes.Unavailable, "start exec (tty): %v", err)
	}
	defer ptmx.Close()

	procDone := make(chan struct{})
	defer close(procDone)
	go killOnDisconnect(stream, cmd, procDone)

	// client -> ptmx. A client CloseSend (io.EOF) just stops the input feed —
	// the command keeps running until it exits on its own (or the client
	// context dies and the watcher above kills it). Interactive clients end a
	// shell with an in-band exit/^D instead of closing the stream.
	go func() {
		for {
			in, rerr := stream.Recv()
			if rerr != nil {
				return
			}
			switch m := in.GetMsg().(type) {
			case *fleetgrpc.ExecIn_Stdin:
				if len(m.Stdin) > 0 {
					if _, werr := ptmx.Write(m.Stdin); werr != nil {
						return
					}
				}
			case *fleetgrpc.ExecIn_Resize:
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(m.Resize.GetRows()), Cols: uint16(m.Resize.GetCols())})
			}
		}
	}()

	// ptmx -> client. When the child exits the master read fails (EIO on
	// Linux, EOF elsewhere) — either way the drain is complete; reap and
	// report. A failed Send means the client is gone, so kill the command
	// rather than block forever on a full PTY buffer.
	buf := make([]byte, execChunkSize)
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			if serr := snd.sendStdout(buf[:n]); serr != nil {
				_ = cmd.Process.Kill()
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	code, errMsg := execExitResult(cmd.Wait())
	return code, errMsg, nil
}

// runExecPipes runs cmd with separate stdin/stdout/stderr pipes. Client EOF
// (CloseSend) closes stdin so commands that read to EOF (cat, sort, …) can
// finish; both output pumps drain fully before Wait (which closes the pipes)
// and before the caller sends the terminal exit frame.
func runExecPipes(stream fleetgrpc.FleetService_ExecServer, snd *execSender, cmd *exec.Cmd) (int32, *string, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, nil, status.Errorf(codes.Internal, "stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, nil, status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, status.Errorf(codes.Unavailable, "start exec: %v", err)
	}

	procDone := make(chan struct{})
	defer close(procDone)
	go killOnDisconnect(stream, cmd, procDone)

	// client -> stdin. Any recv error ends the feed; for the io.EOF of a
	// client CloseSend the deferred close is exactly the half-close the
	// command needs to see EOF. Resize frames are meaningless without a TTY
	// and are ignored (GetStdin returns nil for them).
	go func() {
		defer stdin.Close()
		for {
			in, rerr := stream.Recv()
			if rerr != nil {
				return
			}
			if data := in.GetStdin(); len(data) > 0 {
				if _, werr := stdin.Write(data); werr != nil {
					return
				}
			}
		}
	}()

	// stdout/stderr -> client, concurrently. A failed Send means the client
	// is gone: kill the command so a chatty process can't wedge Wait on a
	// full pipe (the context watcher usually beats us to it).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if !pumpExecOutput(stdout, snd.sendStdout) {
			_ = cmd.Process.Kill()
		}
	}()
	go func() {
		defer wg.Done()
		if !pumpExecOutput(stderr, snd.sendStderr) {
			_ = cmd.Process.Kill()
		}
	}()
	wg.Wait()

	code, errMsg := execExitResult(cmd.Wait())
	return code, errMsg, nil
}

// pumpExecOutput copies r into send frames until the reader is exhausted.
// It reports false when the send side failed (client gone) rather than the
// reader simply draining.
func pumpExecOutput(r io.Reader, send func([]byte) error) bool {
	buf := make([]byte, execChunkSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return false
			}
		}
		if rerr != nil {
			return true
		}
	}
}

// killOnDisconnect kills the exec'd process when the client vanishes (stream
// context done) so commands don't outlive their callers; done releases the
// watcher once the process has been reaped.
func killOnDisconnect(stream fleetgrpc.FleetService_ExecServer, cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-stream.Context().Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	case <-done:
	}
}

// applyExecEnv appends the request's env vars to the command's environment.
// A nil cmd.Env means "inherit the server process environment", so the base
// is materialized first; request vars are appended last and therefore win.
func applyExecEnv(cmd *exec.Cmd, env map[string]string) {
	if len(env) == 0 {
		return
	}
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	for k, v := range env {
		base = append(base, k+"="+v)
	}
	cmd.Env = base
}

// execExitResult maps cmd.Wait's error to the ExecExit fields: a clean run is
// code 0; a command that ran and failed carries its real exit code (-1 if
// signal-killed); anything else (a Wait plumbing failure) is code -1 with the
// error text for the client to surface.
func execExitResult(waitErr error) (int32, *string) {
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return int32(exitErr.ExitCode()), nil
	}
	msg := waitErr.Error()
	return -1, &msg
}

// execSender serializes Sends on the Exec stream: gRPC forbids concurrent
// Send on one ServerStream, and the non-TTY path has three writers (stdout
// pump, stderr pump, terminal exit).
type execSender struct {
	mu     sync.Mutex
	stream fleetgrpc.FleetService_ExecServer
}

func (s *execSender) send(out *fleetgrpc.ExecOut) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(out)
}

func (s *execSender) sendStdout(b []byte) error {
	return s.send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Stdout{Stdout: b}})
}

func (s *execSender) sendStderr(b []byte) error {
	return s.send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Stderr{Stderr: b}})
}

func (s *execSender) sendExit(code int32, errMsg *string) error {
	return s.send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Exit{Exit: &fleetgrpc.ExecExit{Code: code, Error: errMsg}}})
}

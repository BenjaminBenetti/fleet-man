package server

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exec_stream.go is the streaming half of exec (issue #141): one Exec bidi
// stream per command run inside an instance, for REMOTE clients that have no
// local backend and so cannot use the ResolveExecCommand carve-out (exec.go).
// The first client frame must carry ExecStart; subsequent frames carry stdin
// bytes (or TTY resizes). The server starts the command as a backend.ExecStream
// (the backend's own StreamExecer when it has one, else its exec command run
// here on a PTY or pipes; exec_stream_cmd.go), pipes stdin/stdout/stderr (or
// the PTY) over the stream, and finishes with a terminal ExecExit frame
// carrying the command's exit code.

// execChunkSize bounds one output frame. 32 KiB matches forwardChunkSize and
// io.Copy's default buffer; well under gRPC's 4 MB message ceiling.
const execChunkSize = 32 * 1024

// buildExecCommand builds the host-side command that runs argv inside the
// instance's container through b, its backend, for a backend that has no
// StreamExecer. Package var so tests can stub the whole backend layer.
// ExecCommandQuiet, not ExecCommand: the wrapper's completion log only fires
// through its Run/Output methods (which this handler bypasses), and Exec logs
// its own single per-session line instead.
var buildExecCommand = func(b backend.Backend, inst *fleet.Instance, argv []string) *exec.Cmd {
	return b.ExecCommandQuiet(inst.WorkspaceDir, argv).Cmd
}

// openExecStream starts the command start asks for inside inst. Package var so
// tests can stand in any ExecStream.
var openExecStream = func(ctx context.Context, inst *fleet.Instance, start *fleetgrpc.ExecStart) (backend.ExecStream, error) {
	return startExecStream(ctx, backendutil.NewForInstance(inst, false), inst, start)
}

// startExecStream starts start's command through b, inst's backend, as
// ExecCommand runs one (ExecClassExec), with start's env on top. A backend
// that runs commands as streams of its own (StreamExecer; Kubernetes runs them
// in the pod, PTY and process group included) starts it; for any other, the
// host command buildExecCommand builds runs here on a PTY or pipes. A failure
// is the gRPC status Exec returns.
func startExecStream(ctx context.Context, b backend.Backend, inst *fleet.Instance, start *fleetgrpc.ExecStart) (backend.ExecStream, error) {
	if execer, ok := b.(backend.StreamExecer); ok {
		stream, err := execer.OpenExec(ctx, backend.ExecRequest{
			Class:        backend.ExecClassExec,
			WorkspaceDir: inst.WorkspaceDir,
			ContainerID:  inst.ContainerID,
			Argv:         start.GetArgv(),
			Env:          start.GetEnv(),
			TTY:          start.GetTty(),
		})
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "start exec: %v", err)
		}
		return stream, nil
	}
	cmd := buildExecCommand(b, inst, start.GetArgv())
	applyExecEnv(cmd, start.GetEnv())
	return startCmdExecStream(cmd, start.GetTty())
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

	// One per-session log line (the ExecCommandQuiet counterpart of
	// flog.ContainerExec), recorded when the session finishes.
	logArgs := []any{
		"fleet", start.GetFleet(), "instance", start.GetInstance(),
		"cmd", strings.Join(argv, " "), "tty", start.GetTty(),
	}
	began := time.Now()
	proc, err := openExecStream(stream.Context(), inst, start)
	if err != nil {
		flog.Error("exec session", append(logArgs, "ms", flog.MillisSince(began), "err", err)...)
		return err
	}
	snd := &execSender{stream: stream}
	code, errMsg := runExecStream(stream, snd, proc, start.GetTty())
	logArgs = append(logArgs, "code", code, "ms", flog.MillisSince(began))
	if errMsg != nil {
		flog.Error("exec session", append(logArgs, "err", *errMsg)...)
	} else {
		flog.Info("exec session", logArgs...)
	}
	return snd.sendExit(code, errMsg)
}

// runExecStream pipes the running command proc over the stream until it
// exits, and returns the exit for the terminal frame. tty picks the plumbing
// proc was started with. A vanished client (stream context done) or a failed
// Send aborts the command, so commands don't outlive their callers.
func runExecStream(stream fleetgrpc.FleetService_ExecServer, snd *execSender, proc backend.ExecStream, tty bool) (int32, *string) {
	procDone := make(chan struct{})
	defer close(procDone)
	go closeOnDisconnect(stream.Context(), proc, procDone)

	if tty {
		pumpExecTTY(stream, snd, proc)
	} else {
		pumpExecPipes(stream, snd, proc)
	}

	code, err := proc.Wait()
	if err != nil {
		msg := err.Error()
		return int32(code), &msg
	}
	return int32(code), nil
}

// pumpExecTTY pipes a command on a PTY. The PTY merges stdout and stderr, so
// all output travels as stdout frames; resize frames apply to the PTY
// (including a resize that arrives before any output — the recv pump starts as
// soon as the command does).
func pumpExecTTY(stream fleetgrpc.FleetService_ExecServer, snd *execSender, proc backend.ExecStream) {
	// client -> PTY. A client CloseSend (io.EOF) just stops the input feed —
	// the command keeps running until it exits on its own (or the client
	// context dies and the watcher aborts it). Interactive clients end a shell
	// with an in-band exit/^D instead of closing the stream.
	go func() {
		stdin := proc.Stdin()
		for {
			in, rerr := stream.Recv()
			if rerr != nil {
				return
			}
			switch m := in.GetMsg().(type) {
			case *fleetgrpc.ExecIn_Stdin:
				if len(m.Stdin) > 0 {
					if _, werr := stdin.Write(m.Stdin); werr != nil {
						return
					}
				}
			case *fleetgrpc.ExecIn_Resize:
				_ = proc.Resize(uint16(m.Resize.GetRows()), uint16(m.Resize.GetCols()))
			}
		}
	}()

	// PTY -> client. When the command exits the read fails (EIO from a local
	// PTY on Linux, EOF elsewhere) — either way the drain is complete. A
	// failed Send means the client is gone, so abort the command rather than
	// block forever on a full PTY buffer.
	if !pumpExecOutput(proc.Stdout(), snd.sendStdout) {
		_ = proc.Close()
	}
}

// pumpExecPipes pipes a command with separate stdin/stdout/stderr. Client EOF
// (CloseSend) closes stdin so commands that read to EOF (cat, sort, …) can
// finish; both output pumps drain fully before the caller's Wait and before
// it sends the terminal exit frame.
func pumpExecPipes(stream fleetgrpc.FleetService_ExecServer, snd *execSender, proc backend.ExecStream) {
	// client -> stdin. Any recv error ends the feed; for the io.EOF of a
	// client CloseSend the deferred close is exactly the half-close the
	// command needs to see EOF. Resize frames are meaningless without a TTY
	// and are ignored (GetStdin returns nil for them).
	go func() {
		stdin := proc.Stdin()
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
	// is gone: abort the command so a chatty process can't wedge Wait on a
	// full pipe (the context watcher usually beats us to it).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if !pumpExecOutput(proc.Stdout(), snd.sendStdout) {
			_ = proc.Close()
		}
	}()
	go func() {
		defer wg.Done()
		if !pumpExecOutput(proc.Stderr(), snd.sendStderr) {
			_ = proc.Close()
		}
	}()
	wg.Wait()
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

// closeOnDisconnect aborts the command when the client vanishes (stream
// context done) so commands don't outlive their callers; done releases the
// watcher once the command has been waited for.
func closeOnDisconnect(ctx context.Context, proc backend.ExecStream, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = proc.Close()
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

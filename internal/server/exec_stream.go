package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/execserve"
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
	if start.GetTty() {
		ensureInstanceTmux(backendutil.NewForInstance(inst, false))
	}
	// An editor session (VS Code Remote-SSH through `fleet ssh-proxy`) gets
	// the instance's editor settings first, and reports what it applied in
	// the creation log as well as to the caller.
	editor := isEditorSession(argv)
	if editor {
		prepareEditorSession(stream.Context(), start.GetFleet(), inst)
	}
	began := time.Now()
	proc, err := openExecStream(stream.Context(), inst, start)
	if err != nil {
		flog.Error("exec session", append(logArgs, "ms", flog.MillisSince(began), "err", err)...)
		return err
	}
	if editor {
		log := newCreationLogAppender(start.GetFleet(), start.GetInstance())
		defer log.Close()
		proc = withCreationLog(proc, log)
	}
	if start.GetTty() {
		// A terminal attached: the instance's "sessions reset" notice has
		// been seen.
		s.kubePods.attached(start.GetFleet(), start.GetInstance())
	}
	code, errMsg, sendErr := execserve.Serve(stream, proc, start.GetTty())
	logArgs = append(logArgs, "code", code, "ms", flog.MillisSince(began))
	if errMsg != nil {
		flog.Error("exec session", append(logArgs, "err", *errMsg)...)
	} else {
		flog.Info("exec session", logArgs...)
	}
	return sendErr
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

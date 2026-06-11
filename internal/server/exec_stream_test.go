package server

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedExecInstance writes one running instance to the isolated state dir.
func seedExecInstance(t *testing.T) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendDevcontainer, WorkspaceDir: "/ws/alpha/i1", ContainerID: "c1", Status: fleet.StatusRunning},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// execStub records what buildExecCommand was asked for, plus the *exec.Cmd it
// handed back (so tests can watch the spawned process).
type execStub struct {
	Instance string
	Argv     []string
	Cmd      *exec.Cmd
}

// stubExecCommand points buildExecCommand at a plain host exec.Command for the
// duration of the test, so no backend/container is needed: argv runs directly
// on the test host.
func stubExecCommand(t *testing.T) *execStub {
	t.Helper()
	seen := &execStub{}
	orig := buildExecCommand
	buildExecCommand = func(inst *fleet.Instance, argv []string) *exec.Cmd {
		seen.Instance = inst.Name
		seen.Argv = argv
		seen.Cmd = exec.Command(argv[0], argv[1:]...)
		return seen.Cmd
	}
	t.Cleanup(func() { buildExecCommand = orig })
	return seen
}

func execStartFrame(fleetName, instance string, argv []string, tty bool, env map[string]string) *fleetgrpc.ExecIn {
	return &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Start{Start: &fleetgrpc.ExecStart{
		Fleet:    fleetName,
		Instance: instance,
		Argv:     argv,
		Tty:      tty,
		Env:      env,
	}}}
}

func execStdinFrame(data []byte) *fleetgrpc.ExecIn {
	return &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Stdin{Stdin: data}}
}

func execResizeFrame(rows, cols uint32) *fleetgrpc.ExecIn {
	return &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Resize{Resize: &fleetgrpc.ExecResize{Rows: rows, Cols: cols}}}
}

// drainExec reads ExecOut frames until the terminal exit, returning the
// accumulated stdout, stderr, and the exit message.
func drainExec(t *testing.T, stream fleetgrpc.FleetService_ExecClient) ([]byte, []byte, *fleetgrpc.ExecExit) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	for {
		out, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv (stdout %q, stderr %q so far): %v", stdout.String(), stderr.String(), err)
		}
		switch m := out.GetMsg().(type) {
		case *fleetgrpc.ExecOut_Stdout:
			stdout.Write(m.Stdout)
		case *fleetgrpc.ExecOut_Stderr:
			stderr.Write(m.Stderr)
		case *fleetgrpc.ExecOut_Exit:
			return stdout.Bytes(), stderr.Bytes(), m.Exit
		}
	}
}

// TestExecStdoutStderrAndExitCode: stdout and stderr arrive on their own
// channels and the real (non-zero) exit code comes back in the exit frame.
func TestExecStdoutStderrAndExitCode(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	seen := stubExecCommand(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, false, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	stdout, stderr, exit := drainExec(t, stream)
	if string(stdout) != "out\n" {
		t.Errorf("stdout = %q, want %q", stdout, "out\n")
	}
	if string(stderr) != "err\n" {
		t.Errorf("stderr = %q, want %q", stderr, "err\n")
	}
	if exit.GetCode() != 3 {
		t.Errorf("exit code = %d, want 3", exit.GetCode())
	}
	if exit.GetError() != "" {
		t.Errorf("exit error = %q, want empty (a plain exit code is not an error)", exit.GetError())
	}
	if seen.Instance != "i1" {
		t.Errorf("buildExecCommand saw instance %q, want i1", seen.Instance)
	}
}

// TestExecStdinRoundTrip: stdin frames reach the command, and the client's
// CloseSend half-closes stdin so a read-to-EOF command (cat) finishes cleanly.
func TestExecStdinRoundTrip(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	stubExecCommand(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"cat"}, false, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("hello "))); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("fleet\n"))); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	stdout, stderr, exit := drainExec(t, stream)
	if string(stdout) != "hello fleet\n" {
		t.Errorf("stdout = %q, want %q", stdout, "hello fleet\n")
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if exit.GetCode() != 0 {
		t.Errorf("exit code = %d, want 0", exit.GetCode())
	}
}

// TestExecEnvIsApplied: env vars from ExecStart reach the command (appended
// after the inherited environment, so they win).
func TestExecEnvIsApplied(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	stubExecCommand(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	argv := []string{"sh", "-c", `printf '%s' "$FLEET_EXEC_TEST_VAR"`}
	env := map[string]string{"FLEET_EXEC_TEST_VAR": "from-the-request"}
	if err := stream.Send(execStartFrame("alpha", "i1", argv, false, env)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	stdout, _, exit := drainExec(t, stream)
	if string(stdout) != "from-the-request" {
		t.Errorf("stdout = %q, want %q", stdout, "from-the-request")
	}
	if exit.GetCode() != 0 {
		t.Errorf("exit code = %d, want 0", exit.GetCode())
	}
}

// TestExecTTYEchoResizeAndEOF: a TTY session echoes input back (PTY line
// discipline), tolerates a resize that arrives before any output, and ends
// cleanly on an in-band EOF (^D makes cat read EOF and exit 0).
func TestExecTTYEchoResizeAndEOF(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	stubExecCommand(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"cat"}, true, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	// Resize before the command has produced anything must be tolerated.
	if err := stream.Send(execResizeFrame(40, 120)); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("ping\n"))); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	// ^D after a complete line: the PTY's canonical mode turns it into EOF
	// for cat, which exits 0 — a clean in-band session end, no CloseSend.
	if err := stream.Send(execStdinFrame([]byte{0x04})); err != nil {
		t.Fatalf("send EOT: %v", err)
	}

	stdout, stderr, exit := drainExec(t, stream)
	// The PTY echoes the typed line and cat repeats it; both surface as
	// stdout (a PTY merges the streams). Exact framing/CRLF translation
	// varies, so just require the payload to have come back.
	if !strings.Contains(string(stdout), "ping") {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "ping")
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want empty (PTY output is all stdout)", stderr)
	}
	if exit.GetCode() != 0 {
		t.Errorf("exit code = %d, want 0", exit.GetCode())
	}
}

// TestExecTTYClientCancelKillsProcess: when the client vanishes (context
// cancel), the server kills and reaps the command instead of leaking it.
func TestExecTTYClientCancelKillsProcess(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	seen := stubExecCommand(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"cat"}, true, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("ready\n"))); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	// Wait for the echo so the process is provably up before we cancel.
	var got bytes.Buffer
	for !strings.Contains(got.String(), "ready") {
		out, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv waiting for echo (got %q): %v", got.String(), err)
		}
		got.Write(out.GetStdout())
	}
	pid := seen.Cmd.Process.Pid

	cancel()

	// The handler kills the process and Wait reaps it, after which signal 0
	// reports ESRCH. Generous deadline; no sleep-based timing assumptions.
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive/unreaped after client cancel", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExecRejectsBadStart: a non-start first frame and an empty argv are
// InvalidArgument; an unknown fleet is NotFound.
func TestExecRejectsBadStart(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	stubExecCommand(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First frame carries stdin instead of start.
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("no start"))); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("first-frame-not-start: want InvalidArgument, got %v", err)
	}

	// Empty argv.
	stream, err = client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", nil, false, nil)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty argv: want InvalidArgument, got %v", err)
	}

	// Unknown fleet.
	stream, err = client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("ghost", "i1", []string{"true"}, false, nil)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("unknown fleet: want NotFound, got %v", err)
	}
}

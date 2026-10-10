package server

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exec_stream_execer_test.go: the Exec RPC over a backend that runs commands
// as streams of its own (backend.StreamExecer).

// refuseHostExec fails the test if the Exec RPC builds a host command.
func refuseHostExec(t *testing.T) {
	t.Helper()
	orig := buildExecCommand
	buildExecCommand = func(backend.Backend, *fleet.Instance, []string) *exec.Cmd {
		t.Error("a StreamExecer backend's command was built as a host command")
		return exec.Command("true")
	}
	t.Cleanup(func() { buildExecCommand = orig })
}

// stubStreamExecer routes the Exec RPC's commands to a streamExecer backend.
func stubStreamExecer(t *testing.T) *streamExecer {
	t.Helper()
	refuseHostExec(t)
	b := &streamExecer{}
	orig := openExecStream
	openExecStream = func(ctx context.Context, inst *fleet.Instance, start *fleetgrpc.ExecStart) (backend.ExecStream, error) {
		return startExecStream(ctx, b, inst, start)
	}
	t.Cleanup(func() { openExecStream = orig })
	return b
}

// openedStream waits for the backend's first stream.
func openedStream(t *testing.T, b *streamExecer) *fakeExecStream {
	t.Helper()
	var stream *fakeExecStream
	waitFor(t, "the command to be opened", func() bool {
		_, streams := b.opened()
		if len(streams) > 0 {
			stream = streams[0]
		}
		return stream != nil
	})
	return stream
}

// TestStartExecStreamPrefersStreamExecer: a StreamExecer backend runs the
// command itself, as ExecCommand would (ExecClassExec), with the request's
// argv, env and TTY; nothing is built as a host command. A failed open is
// Unavailable.
func TestStartExecStreamPrefersStreamExecer(t *testing.T) {
	refuseHostExec(t)
	b := &streamExecer{}
	inst := &fleet.Instance{Name: "i1", WorkspaceDir: "/ws/alpha/i1", ContainerID: "c1"}
	start := &fleetgrpc.ExecStart{Argv: []string{"bash", "-l"}, Tty: true, Env: map[string]string{"TERM": "xterm-256color"}}

	proc, err := startExecStream(context.Background(), b, inst, start)
	if err != nil {
		t.Fatalf("startExecStream: %v", err)
	}
	reqs, streams := b.opened()
	want := backend.ExecRequest{
		Class:        backend.ExecClassExec,
		WorkspaceDir: "/ws/alpha/i1",
		ContainerID:  "c1",
		Argv:         []string{"bash", "-l"},
		Env:          map[string]string{"TERM": "xterm-256color"},
		TTY:          true,
	}
	if len(reqs) != 1 || !reflect.DeepEqual(reqs[0], want) {
		t.Fatalf("OpenExec requests = %+v, want [%+v]", reqs, want)
	}
	if proc != backend.ExecStream(streams[0]) {
		t.Fatal("startExecStream did not return the backend's stream")
	}

	b = &streamExecer{openErr: errors.New("pod not ready")}
	if _, err := startExecStream(context.Background(), b, inst, start); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "pod not ready") {
		t.Fatalf("failed open = %v, want Unavailable naming the cause", err)
	}
}

// TestExecOverAStreamExecerPipes: without a TTY, stdin frames reach the
// command, CloseSend half-closes its stdin, stdout and stderr come back on
// their own channels, and the exit code ends the stream.
func TestExecOverAStreamExecerPipes(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	b := stubStreamExecer(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"sort"}, false, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("abc"))); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	proc := openedStream(t, b)
	go func() {
		in, _ := io.ReadAll(proc.cmdStdin) // ends at the half-close
		_, _ = proc.cmdStdout.Write(append([]byte("got:"), in...))
		_, _ = proc.cmdStderr.Write([]byte("warn"))
		proc.exit(7, nil)
	}()

	stdout, stderr, exit := drainExec(t, stream)
	if string(stdout) != "got:abc" || string(stderr) != "warn" {
		t.Errorf("stdout %q, stderr %q; want %q, %q", stdout, stderr, "got:abc", "warn")
	}
	if exit.GetCode() != 7 || exit.GetError() != "" {
		t.Errorf("exit = %d %q, want 7 and no error", exit.GetCode(), exit.GetError())
	}
	if reqs, _ := b.opened(); reqs[0].TTY || reqs[0].Class != backend.ExecClassExec {
		t.Errorf("request = %+v, want a non-TTY ExecClassExec command", reqs[0])
	}
}

// TestExecOverAStreamExecerTTY: with a TTY, resizes reach the stream and
// output comes back as stdout; a stream that breaks without an exit status
// ends with code -1 and the error.
func TestExecOverAStreamExecerTTY(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	b := stubStreamExecer(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"bash"}, true, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(execResizeFrame(40, 120)); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	if err := stream.Send(execStdinFrame([]byte("hi"))); err != nil {
		t.Fatalf("send stdin: %v", err)
	}

	proc := openedStream(t, b)
	// Frames are handled in order: the resize lands before the input does.
	waitFor(t, "the resize", func() bool {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return len(proc.resizes) > 0
	})
	go func() {
		in := make([]byte, 2)
		_, _ = io.ReadFull(proc.cmdStdin, in)
		_, _ = proc.cmdStdout.Write(in)
		proc.exit(-1, errors.New("agent session lost"))
	}()

	stdout, stderr, exit := drainExec(t, stream)
	if string(stdout) != "hi" || len(stderr) != 0 {
		t.Errorf("stdout %q, stderr %q; want the echo on stdout only", stdout, stderr)
	}
	if exit.GetCode() != -1 || exit.GetError() != "agent session lost" {
		t.Errorf("exit = %d %q, want -1 and the stream's error", exit.GetCode(), exit.GetError())
	}
	proc.mu.Lock()
	resizes := proc.resizes
	proc.mu.Unlock()
	if len(resizes) != 1 || resizes[0] != [2]uint16{40, 120} {
		t.Errorf("resizes = %v, want [[40 120]]", resizes)
	}
}

// TestExecOverAStreamExecerClientGoneAborts: a client that vanishes aborts the
// command (Close) and the handler still waits for it.
func TestExecOverAStreamExecerClientGoneAborts(t *testing.T) {
	isolateFleetDir(t)
	seedExecInstance(t)
	b := stubStreamExecer(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := stream.Send(execStartFrame("alpha", "i1", []string{"sleep", "infinity"}, false, nil)); err != nil {
		t.Fatalf("send start: %v", err)
	}
	proc := openedStream(t, b)

	cancel()
	waitFor(t, "the command to be aborted and waited for", func() bool {
		closes, waits := proc.counts()
		return closes > 0 && waits == 1
	})
}

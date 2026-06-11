package execstream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeStream is a hand-rolled exec stream: it records Send/CloseSend and
// replays scripted ExecOut frames from Recv. When gated, Recv blocks until
// CloseSend — that deterministically orders the stdin pump (which ends with
// CloseSend) before any output is delivered, so tests can assert on the sent
// frames without racing the pump goroutine.
type fakeStream struct {
	mu     sync.Mutex
	sent   []*fleetgrpc.ExecIn
	closed bool

	outs    []*fleetgrpc.ExecOut
	recvIdx int
	recvErr error // returned once outs are drained; nil means io.EOF

	gate chan struct{} // non-nil: Recv waits for CloseSend
}

func newGatedFake(outs ...*fleetgrpc.ExecOut) *fakeStream {
	return &fakeStream{outs: outs, gate: make(chan struct{})}
}

func (f *fakeStream) Send(in *fleetgrpc.ExecIn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, in)
	return nil
}

func (f *fakeStream) Recv() (*fleetgrpc.ExecOut, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recvIdx < len(f.outs) {
		out := f.outs[f.recvIdx]
		f.recvIdx++
		return out, nil
	}
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	return nil, io.EOF
}

func (f *fakeStream) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed && f.gate != nil {
		close(f.gate)
	}
	f.closed = true
	return nil
}

// Frame constructors.

func outStdout(s string) *fleetgrpc.ExecOut {
	return &fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Stdout{Stdout: []byte(s)}}
}

func outStderr(s string) *fleetgrpc.ExecOut {
	return &fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Stderr{Stderr: []byte(s)}}
}

func outExit(code int32, errMsg string) *fleetgrpc.ExecOut {
	exit := &fleetgrpc.ExecExit{Code: code}
	if errMsg != "" {
		exit.Error = &errMsg
	}
	return &fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Exit{Exit: exit}}
}

func TestRunCollectsStdoutAndExitCode(t *testing.T) {
	fake := newGatedFake(outStdout("foo"), outStdout("bar"), outExit(7, ""))
	var stdout bytes.Buffer

	code, err := run(fake, Options{
		Fleet:    "myfleet",
		Instance: "inst-1",
		Argv:     []string{"sh", "-c", "echo hi"},
		Stdout:   &stdout,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if got := stdout.String(); got != "foobar" {
		t.Fatalf("stdout = %q, want %q", got, "foobar")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.closed {
		t.Fatal("CloseSend was not called for nil stdin")
	}
	if len(fake.sent) == 0 {
		t.Fatal("no frames sent")
	}
	start := fake.sent[0].GetStart()
	if start == nil {
		t.Fatalf("first frame is not start: %v", fake.sent[0])
	}
	if start.GetFleet() != "myfleet" || start.GetInstance() != "inst-1" {
		t.Fatalf("start target = %s/%s, want myfleet/inst-1", start.GetFleet(), start.GetInstance())
	}
	if got := strings.Join(start.GetArgv(), " "); got != "sh -c echo hi" {
		t.Fatalf("start argv = %q", got)
	}
	if start.GetTty() {
		t.Fatal("start tty = true, want false")
	}
}

func TestRunRoutesStderrSeparately(t *testing.T) {
	fake := newGatedFake(outStdout("out"), outStderr("err"), outExit(0, ""))
	var stdout, stderr bytes.Buffer

	code, err := run(fake, Options{
		Fleet: "f", Instance: "i", Argv: []string{"true"},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil || code != 0 {
		t.Fatalf("run = (%d, %v), want (0, nil)", code, err)
	}
	if stdout.String() != "out" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "out")
	}
	if stderr.String() != "err" {
		t.Fatalf("stderr = %q, want %q", stderr.String(), "err")
	}
}

func TestRunPumpsStdinThenClosesSend(t *testing.T) {
	fake := newGatedFake(outExit(0, ""))

	code, err := run(fake, Options{
		Fleet: "f", Instance: "i", Argv: []string{"cat"},
		Stdin: strings.NewReader("hello"),
	})
	if err != nil || code != 0 {
		t.Fatalf("run = (%d, %v), want (0, nil)", code, err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.closed {
		t.Fatal("CloseSend was not called after stdin EOF")
	}
	var stdin []byte
	for _, in := range fake.sent[1:] {
		stdin = append(stdin, in.GetStdin()...)
	}
	if string(stdin) != "hello" {
		t.Fatalf("stdin frames = %q, want %q", stdin, "hello")
	}
}

func TestRunPropagatesExitError(t *testing.T) {
	fake := newGatedFake(outExit(3, "command blew up"))

	code, err := run(fake, Options{Fleet: "f", Instance: "i", Argv: []string{"false"}})
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if err == nil || err.Error() != "command blew up" {
		t.Fatalf("err = %v, want %q", err, "command blew up")
	}
}

func TestRunMapsUnimplemented(t *testing.T) {
	fake := newGatedFake()
	fake.recvErr = status.Error(codes.Unimplemented, "method Exec not implemented")

	_, err := run(fake, Options{Fleet: "f", Instance: "i", Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "upgrade fleetd") {
		t.Fatalf("err = %v, want the upgrade-fleetd message", err)
	}
}

func TestRunErrorsOnEOFWithoutExit(t *testing.T) {
	fake := newGatedFake(outStdout("partial"))

	_, err := run(fake, Options{Fleet: "f", Instance: "i", Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "without an exit status") {
		t.Fatalf("err = %v, want missing-exit-status error", err)
	}
}

// --- Output (via a fake FleetServiceClient) --------------------------------

// fakeFullStream upgrades fakeStream to the full generated client-stream
// interface so it can be returned from a fake service's Exec.
type fakeFullStream struct {
	fakeStream
}

func (f *fakeFullStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeFullStream) Trailer() metadata.MD         { return nil }
func (f *fakeFullStream) Context() context.Context     { return context.Background() }
func (f *fakeFullStream) SendMsg(any) error            { return nil }
func (f *fakeFullStream) RecvMsg(any) error            { return nil }

// fakeService implements only Exec; the embedded nil interface panics on any
// other method, which is exactly what a one-shot must not call.
type fakeService struct {
	fleetgrpc.FleetServiceClient
	stream *fakeFullStream
}

func (s *fakeService) Exec(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[fleetgrpc.ExecIn, fleetgrpc.ExecOut], error) {
	return s.stream, nil
}

func TestOutputCollectsStdoutAndExitCode(t *testing.T) {
	full := &fakeFullStream{}
	full.outs = []*fleetgrpc.ExecOut{outStdout("session-1\nsession-2\n"), outExit(2, "")}
	full.gate = make(chan struct{})
	svc := &fakeService{stream: full}

	out, code, err := Output(context.Background(), svc, "myfleet", "inst-1", []string{"tmux", "ls"})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if string(out) != "session-1\nsession-2\n" {
		t.Fatalf("stdout = %q", out)
	}

	full.mu.Lock()
	defer full.mu.Unlock()
	start := full.sent[0].GetStart()
	if start == nil || start.GetTty() {
		t.Fatalf("start frame = %v, want non-TTY start", full.sent[0])
	}
	if !full.closed {
		t.Fatal("CloseSend was not called (one-shot must half-close immediately)")
	}
}

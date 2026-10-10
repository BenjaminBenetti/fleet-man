package execserve

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// fakeStream is a server Exec-shaped stream: Recv replays scripted ExecIn,
// Send records every ExecOut.
type fakeStream struct {
	ctx  context.Context
	in   chan *fleetgrpc.ExecIn
	mu   sync.Mutex
	sent []*fleetgrpc.ExecOut
}

func newFakeStream(ctx context.Context) *fakeStream {
	return &fakeStream{ctx: ctx, in: make(chan *fleetgrpc.ExecIn, 16)}
}
func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) Recv() (*fleetgrpc.ExecIn, error) {
	in, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return in, nil
}
func (f *fakeStream) Send(out *fleetgrpc.ExecOut) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, out)
	return nil
}
func (f *fakeStream) stdin(b string) {
	f.in <- &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Stdin{Stdin: []byte(b)}}
}
func (f *fakeStream) resize(r, c uint32) {
	f.in <- &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Resize{Resize: &fleetgrpc.ExecResize{Rows: r, Cols: c}}}
}
func (f *fakeStream) closeSend() { close(f.in) }

func (f *fakeStream) out() (stdout, stderr string, code int32, errMsg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.sent {
		switch m := o.GetMsg().(type) {
		case *fleetgrpc.ExecOut_Stdout:
			stdout += string(m.Stdout)
		case *fleetgrpc.ExecOut_Stderr:
			stderr += string(m.Stderr)
		case *fleetgrpc.ExecOut_Exit:
			code = m.Exit.GetCode()
			errMsg = m.Exit.GetError()
		}
	}
	return
}

// fakeProc is a backend.ExecStream driven by the test.
type fakeProc struct {
	stdin   *io.PipeReader
	stdinW  *io.PipeWriter
	stdout  *io.PipeReader
	stdoutW *io.PipeWriter
	stderr  *io.PipeReader
	stderrW *io.PipeWriter

	mu      sync.Mutex
	resizes []string
	done    chan struct{}
	code    int
	werr    error
	closed  bool
}

func newFakeProc() *fakeProc {
	p := &fakeProc{done: make(chan struct{})}
	p.stdin, p.stdinW = io.Pipe()
	p.stdout, p.stdoutW = io.Pipe()
	p.stderr, p.stderrW = io.Pipe()
	return p
}
func (p *fakeProc) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeProc) Stdout() io.Reader     { return p.stdout }
func (p *fakeProc) Stderr() io.Reader     { return p.stderr }
func (p *fakeProc) Resize(r, c uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizes = append(p.resizes, strconv.Itoa(int(r))+"x"+strconv.Itoa(int(c)))
	return nil
}
func (p *fakeProc) Wait() (int, error) { <-p.done; return p.code, p.werr }
func (p *fakeProc) Close() error {
	p.mu.Lock()
	wasClosed := p.closed
	p.closed = true
	p.mu.Unlock()
	if !wasClosed {
		_ = p.stdoutW.Close()
		_ = p.stderrW.Close()
		close(p.done)
	}
	return nil
}
func (p *fakeProc) exit(code int, err error) {
	p.code, p.werr = code, err
	_ = p.stdoutW.Close()
	_ = p.stderrW.Close()
	close(p.done)
}

// TestServePipesStreamsAndExit: a non-TTY command's stdout and stderr reach
// the client, stdin is pumped and half-closed on CloseSend, and the exit
// frame carries the code.
func TestServePipesStreamsAndExit(t *testing.T) {
	st := newFakeStream(context.Background())
	proc := newFakeProc()
	var gotStdin strings.Builder
	stdinDone := make(chan struct{})
	go func() { _, _ = io.Copy(&gotStdin, proc.stdin); close(stdinDone) }()
	done := make(chan struct{})
	go func() {
		code, errMsg, err := Serve(st, proc, false)
		if err != nil || code != 7 || errMsg != nil {
			t.Errorf("Serve returned code=%d errMsg=%v err=%v", code, errMsg, err)
		}
		close(done)
	}()
	st.stdin("hello")
	st.closeSend()
	_, _ = proc.stdoutW.Write([]byte("out"))
	_, _ = proc.stderrW.Write([]byte("err"))
	proc.exit(7, nil)
	<-done
	<-stdinDone
	stdout, stderr, code, _ := st.out()
	if stdout != "out" || stderr != "err" || code != 7 {
		t.Fatalf("stdout=%q stderr=%q code=%d", stdout, stderr, code)
	}
	if got := gotStdin.String(); got != "hello" {
		t.Fatalf("stdin reached the command as %q", got)
	}
}

// TestServeTTYMergesAndResizes: a TTY command's output is all stdout, resizes
// reach it, and a Wait error becomes the exit frame's error.
func TestServeTTYMergesAndResizes(t *testing.T) {
	st := newFakeStream(context.Background())
	proc := newFakeProc()
	go func() { _, _ = io.Copy(io.Discard, proc.stdin) }()
	done := make(chan struct{})
	go func() {
		code, errMsg, _ := Serve(st, proc, true)
		if code != -1 || errMsg == nil || *errMsg != "boom" {
			t.Errorf("code=%d errMsg=%v", code, errMsg)
		}
		close(done)
	}()
	st.resize(40, 132)
	// Give the resize time to apply before exit.
	for i := 0; i < 100; i++ {
		proc.mu.Lock()
		n := len(proc.resizes)
		proc.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _ = proc.stdoutW.Write([]byte("tty-out"))
	proc.exit(-1, errors.New("boom"))
	<-done
	stdout, _, _, errMsg := st.out()
	if stdout != "tty-out" || errMsg != "boom" {
		t.Fatalf("stdout=%q errMsg=%q", stdout, errMsg)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if len(proc.resizes) == 0 || proc.resizes[0] != "40x132" {
		t.Fatalf("resizes = %v, want 40x132", proc.resizes)
	}
}

// TestServeAbortsWhenClientGone: the stream's context ending aborts the
// command (Close), so a command does not outlive its caller.
func TestServeAbortsWhenClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := newFakeStream(ctx)
	proc := newFakeProc()
	go func() { _, _ = io.Copy(io.Discard, proc.stdin) }()
	done := make(chan struct{})
	go func() { _, _, _ = Serve(st, proc, false); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the client vanished")
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if !proc.closed {
		t.Fatal("the command was not aborted when the client vanished")
	}
}

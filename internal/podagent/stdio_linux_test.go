//go:build linux

package podagent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// directExec is a `pod-agent exec --stdio` process: its stdin and stdout are
// the transport, as a kube exec's are.
type directExec struct {
	cmd    *exec.Cmd
	exited chan error
	io.Reader
	io.WriteCloser
}

func startDirect(t *testing.T, d testDirs, env ...string) *directExec {
	t.Helper()
	cmd := exec.Command(os.Args[0], "pod-agent", "exec", "--stdio")
	cmd.Env = d.helperEnviron("stdio", env...)
	cmd.Stderr = testWriter{t}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	x := &directExec{cmd: cmd, exited: make(chan error, 1), Reader: stdout, WriteCloser: stdin}
	go func() { x.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return x
}

func directHeader(script string) podwire.ExecHeader {
	h := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh", "-c", script}, TTY: true, Rows: 24, Cols: 80}}
	h.Resumable, h.PodUID = true, "pod-uid-1"
	return h
}

// TestExecStdioResumesAfterADroppedExec runs a TTY attach the way the kube-exec
// shim does, kills the exec process the way containerd does when an SPDY exec
// stream is cut, and re-attaches through a new exec: the shell kept running
// and every keystroke arrives once.
func TestExecStdioResumesAfterADroppedExec(t *testing.T) {
	d := newTestDirs(t)
	x1 := startDirect(t, d)
	ctx := context.Background()
	e, err := podwire.OpenExec(ctx, x1, directHeader(`stty size; while read x; do echo "got:$x"; done`), podwire.ExecOptions{Heartbeat: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	out := collect(e.Stdout())
	out.waitFor(t, func(s string) bool { return strings.Contains(s, "24 80") }, 5*time.Second)
	_, _ = e.Stdin().Write([]byte("a\r"))
	out.waitFor(t, func(s string) bool { return strings.Contains(s, "got:a") }, 5*time.Second)

	_ = x1.cmd.Process.Kill()
	waitDetached(t, e)
	if !alive(pid) {
		t.Fatal("the shell died with its exec")
	}
	_, _ = e.Stdin().Write([]byte("b\r"))

	x2 := startDirect(t, d)
	if err := e.ReattachOver(ctx, x2); err != nil {
		t.Fatal(err)
	}
	got := out.waitFor(t, func(s string) bool { return strings.Contains(s, "got:b") }, 5*time.Second)
	_, _ = e.Stdin().Write([]byte{4}) // ^D ends the read loop
	if code, err := e.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
	if strings.Count(got, "got:a") != 1 || strings.Count(got, "got:b") != 1 {
		t.Fatalf("output %q", got)
	}
	select {
	case err := <-x2.exited:
		if err != nil {
			t.Fatalf("exec --stdio exited %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec --stdio did not exit after the command")
	}
	waitGone(t, pid, 2*time.Second)
}

// TestExecStdioHeldOnSilence: a direct exec that hears nothing for the
// stream silence deadline counts as dropped: it exits and the command is held.
func TestExecStdioHeldOnSilence(t *testing.T) {
	d := newTestDirs(t)
	x1 := startDirect(t, d, "PODAGENT_TEST_STREAM_PING=50ms", "PODAGENT_TEST_STREAM_SILENCE=300ms")
	ctx := context.Background()
	e, err := podwire.OpenExec(ctx, x1, directHeader(`read x; echo "late:$x"`), podwire.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-x1.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("a silent direct exec stayed up")
	}
	waitDetached(t, e)
	if heldCount(d.run) != 1 {
		t.Fatalf("held: %d", heldCount(d.run))
	}
	x2 := startDirect(t, d)
	if err := e.ReattachOver(ctx, x2); err != nil {
		t.Fatal(err)
	}
	_, _ = e.Stdin().Write([]byte("v\r"))
	readUntil(t, e.Stdout(), "late:v", 5*time.Second)
	if code, err := e.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
}

func TestExecStdioRefuses(t *testing.T) {
	d := newTestDirs(t)
	ctx := context.Background()

	// No such held stream: gone, whether named in the header or by --resume.
	h := directHeader("")
	h.Request = execas.Request{Class: execas.ClassExec}
	h.Resume = strings.Repeat("1", 32)
	if _, err := podwire.OpenExec(ctx, startDirect(t, d), h, podwire.ExecOptions{}); !errors.Is(err, podwire.ErrHeldStreamGone) {
		t.Fatalf("unknown id: %v", err)
	}
	h.Resume = ""
	x := startDirect(t, d, "PODAGENT_TEST_RESUME="+strings.Repeat("2", 32))
	if err := podwire.WritePreamble(x, podwire.TagExec, h); err != nil {
		t.Fatal(err)
	}
	f, err := podwire.NewFrameReader(x).Next()
	var reply podwire.OpenReply
	if err != nil || podwire.DecodeFrame(f, &reply) != nil || !reply.Gone {
		t.Fatalf("--resume of an unknown id: %v %+v", err, reply)
	}

	// No common protocol version.
	h = directHeader("true")
	h.ProtoMin, h.ProtoMax = podwire.ProtoMax+1, podwire.ProtoMax+1
	_, err = podwire.OpenExec(ctx, startDirect(t, d), h, podwire.ExecOptions{})
	var se *podwire.StartError
	if !errors.As(err, &se) || se.Code != execas.ExitFailed || !strings.Contains(se.Message, "no common protocol version") {
		t.Fatalf("newer client: %v", err)
	}

	// A start failure carries docker's code.
	h = directHeader("")
	h.Argv = []string{"no-such-command-here"}
	if _, err = podwire.OpenExec(ctx, startDirect(t, d), h, podwire.ExecOptions{}); !errors.As(err, &se) || se.Code != execas.ExitNotFound {
		t.Fatalf("missing command: %v", err)
	}
}

// gateWriter passes writes on until it is shut, then blocks them until it is
// released: the output side of an exec stream containerd stopped reading.
type gateWriter struct {
	w        io.Writer
	shut     chan struct{}
	released chan struct{}
	once     sync.Once
}

func (g *gateWriter) Write(p []byte) (int, error) {
	select {
	case <-g.shut:
		<-g.released
		return 0, io.ErrClosedPipe
	default:
	}
	return g.w.Write(p)
}

// TestExecStdioEndsWhileItsOutputIsBlocked: a direct exec whose stream drops
// while a write of the command's output is blocked (containerd stops reading
// a cut stream) still ends at once, and the command is held, whether its
// stdin ends without ABORT or just goes silent.
func TestExecStdioEndsWhileItsOutputIsBlocked(t *testing.T) {
	for _, silent := range []bool{false, true} {
		testExecStdioEndsWhileItsOutputIsBlocked(t, silent)
	}
}

func testExecStdioEndsWhileItsOutputIsBlocked(t *testing.T, silent bool) {
	d := newTestDirs(t)
	o := d.options()
	o.StreamPing, o.StreamSilence = 50*time.Millisecond, 300*time.Millisecond
	stdinR, stdinW := io.Pipe()
	outR, outW := io.Pipe()
	gate := &gateWriter{w: outW, shut: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(func() { close(gate.released); _ = outR.Close(); _ = stdinW.Close() })
	returned := make(chan error, 1)
	go func() { returned <- ExecStdio(context.Background(), stdinR, gate, o, "") }()

	h := directHeader(`yes flood`)
	h.TTY, h.Rows, h.Cols = false, 0, 0
	e, err := podwire.OpenExec(context.Background(), struct {
		io.Reader
		io.WriteCloser
	}{outR, stdinW}, h, podwire.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	readUntil(t, e.Stdout(), "flood", 5*time.Second)
	close(gate.shut)
	time.Sleep(100 * time.Millisecond) // the relay's next output write blocks
	if !silent {
		_ = stdinW.Close()
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("silent %v: exec --stdio stayed up behind a blocked output write", silent)
	}
	deadline := time.Now().Add(3 * time.Second)
	for heldCount(d.run) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if heldCount(d.run) != 1 || !alive(pid) {
		t.Fatalf("silent %v: held %d, alive %v", silent, heldCount(d.run), alive(pid))
	}
}

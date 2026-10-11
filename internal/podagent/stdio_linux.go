//go:build linux

package podagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// ExecStdio is `pod-agent exec --stdio`: one command over the exec's own
// stdio, the direct-mode attach fleet's kube-exec shim makes without fleetd.
// stdin carries the client's preamble (an ExecHeader with its protocol range)
// and frames; stdout carries the agent's. The command runs under a shepherd,
// so when this exec's stream drops (no ABORT: stdin just ends, or nothing
// arrives for the stream silence deadline) a resumable command is held, and
// `pod-agent exec --stdio --resume <id>` (resume, or the header's Resume)
// re-attaches it. Both sides send a PING every stream ping interval.
func ExecStdio(ctx context.Context, stdin io.Reader, stdout io.Writer, opts Options, resume string) error {
	o := opts.withDefaults()
	log := newStderrLogger(o.Stderr)
	takeInitOOMScore(o, log)
	w := podwire.NewFrameWriter(stdout)
	open := func(r podwire.OpenReply) { _ = w.WriteJSON(podwire.FrameOpen, r) }

	type preamble struct {
		tag podwire.Tag
		raw []byte
		err error
	}
	got := make(chan preamble, 1)
	go func() {
		tag, raw, err := podwire.ReadPreamble(stdin)
		got <- preamble{tag, raw, err}
	}()
	var pre preamble
	select {
	case pre = <-got:
	case <-time.After(podwire.HeaderDeadline):
		return errNoHeader
	case <-ctx.Done():
		return ctx.Err()
	}
	if pre.err != nil {
		return fmt.Errorf("read the exec header: %w", pre.err)
	}
	if pre.tag != podwire.TagExec {
		return fmt.Errorf("stdin carries a %s stream, not an exec", pre.tag)
	}
	var h podwire.ExecHeader
	if err := podwire.DecodeHeader(pre.raw, &h); err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitFailed})
		return err
	}
	if resume != "" {
		if h.Resume != "" && h.Resume != resume {
			open(podwire.OpenReply{Error: "--resume and the header name different streams", Code: execas.ExitFailed})
			return fmt.Errorf("--resume %s and header resume %s differ", resume, h.Resume)
		}
		h.Resume = resume
	}
	if h.ProtoMin == 0 && h.ProtoMax == 0 {
		h.ProtoMin, h.ProtoMax = podwire.ProtoMin, podwire.ProtoMin
	}
	proto, err := podwire.Negotiate(podwire.ProtoMin, podwire.ProtoMax, h.ProtoMin, h.ProtoMax)
	if err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitFailed})
		return err
	}
	if err := h.Validate(); err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitFailed, Proto: proto})
		return err
	}
	cl := relayClient{
		r: podwire.NewFrameReader(stdin), w: w, c: closerFunc(func() error { return closeIfCloser(stdout) }),
		ping: o.StreamPing, silence: o.StreamSilence,
	}
	if h.Resume != "" {
		resumeHeld(o, cl, h, proto)
		return nil
	}
	if o.PodUID != "" && h.PodUID != "" && h.PodUID != o.PodUID {
		// The shim verified another pod (by its uid) than the one its exec
		// reached: this pod replaced it since, unverified.
		open(podwire.OpenReply{Refused: podwire.RefusedOtherPod, Error: fmt.Sprintf("this is pod %s, not %s", o.PodUID, h.PodUID), Proto: proto})
		return nil
	}
	host := loadHost(o, log)
	plan, err := host.Plan(ctx, h.Request)
	if err != nil {
		open(podwire.OpenReply{Error: err.Error(), Code: execas.ExitCode(err), Proto: proto})
		return nil
	}
	plan.Cgroup = userCgroup(o)
	startHeld(o, log, cl, h, plan, proto)
	return nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func closeIfCloser(w io.Writer) error {
	if c, ok := w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// TakeStdio is this process's stdin and stdout as the agent's transport, moved
// off fds 0 and 1 (which then read and write /dev/null), so nothing written
// to stdout by accident and no inherited descriptor can corrupt the session.
func TakeStdio() (io.ReadWriteCloser, error) {
	in, err := syscall.Dup(0)
	if err != nil {
		return nil, fmt.Errorf("dup stdin: %w", err)
	}
	out, err := syscall.Dup(1)
	if err != nil {
		_ = syscall.Close(in)
		return nil, fmt.Errorf("dup stdout: %w", err)
	}
	syscall.CloseOnExec(in)
	syscall.CloseOnExec(out)
	// Non-blocking, so the files are in Go's poller and closing them ends a
	// read in progress: the session's Close waits for its reader, which
	// would otherwise sit in read(2) until fleetd's end closed. Nothing else
	// shares these descriptions once 0 and 1 point at /dev/null.
	_ = syscall.SetNonblock(in, true)
	_ = syscall.SetNonblock(out, true)
	if null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		_ = syscall.Dup3(int(null.Fd()), 0, 0)
		_ = syscall.Dup3(int(null.Fd()), 1, 0)
		_ = null.Close()
	}
	return stdioConn{r: os.NewFile(uintptr(in), "agent-stdin"), w: os.NewFile(uintptr(out), "agent-stdout")}, nil
}

type stdioConn struct {
	r *os.File
	w *os.File
}

func (c stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c stdioConn) Close() error {
	_ = c.w.Close()
	return c.r.Close()
}

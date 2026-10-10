//go:build linux

package podagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// collector reads a stream to its end in the background.
type collector struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func collect(r io.Reader) *collector {
	c := &collector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		b := make([]byte, 32<<10)
		for {
			n, err := r.Read(b)
			c.mu.Lock()
			c.buf.Write(b[:n])
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

func (c *collector) waitFor(t *testing.T, cond func(string) bool, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		c.mu.Lock()
		s := c.buf.String()
		c.mu.Unlock()
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("collected %d bytes, condition not met within %v (tail %q)", len(s), within, tail(s))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func tail(s string) string {
	if len(s) > 80 {
		return s[len(s)-80:]
	}
	return s
}

func resumable(h podwire.ExecHeader) podwire.ExecHeader {
	h.Resumable = true
	return h
}

func waitDetached(t *testing.T, e *podwire.Exec) {
	t.Helper()
	ch := e.Detached()
	if ch == nil {
		deadline := time.Now().Add(5 * time.Second)
		for ch == nil && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			ch = e.Detached()
		}
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the command was not detached")
	}
}

// TestResumableSurvivesTransportLoss cuts the session while a command prints
// twice the hold buffer, writes input while detached, and re-attaches through
// another agent: the command never stopped, and the client gets exactly the
// command's output and the command exactly the client's input, once each.
func TestResumableSurvivesTransportLoss(t *testing.T) {
	d := newTestDirs(t)
	s1 := startAgent(t, d.options())
	e, err := s1.c.Exec(context.Background(), resumable(sh(`read x; seq 1 300000; read y; echo "done-$x-$y"`)))
	if err != nil {
		t.Fatal(err)
	}
	if !podwire.ValidResumeID(e.ResumeID()) || !e.Resumable() {
		t.Fatalf("resume id %q", e.ResumeID())
	}
	out := collect(e.Stdout())
	if _, err := e.Stdin().Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, func(s string) bool { return len(s) > 100<<10 }, 10*time.Second)
	pid := e.PID()

	s1.cut()
	waitDetached(t, e)
	if !alive(pid) {
		t.Fatal("the held command died with its transport")
	}
	// Written while detached: kept, and sent on the re-attach.
	if _, err := e.Stdin().Write([]byte("bye\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the command fill the hold buffer
	if heldCount(d.run) != 1 {
		t.Fatalf("held markers: %d", heldCount(d.run))
	}

	s2 := startAgent(t, d.options())
	if err := s2.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	code, err := e.Wait()
	if err != nil || code != 0 {
		t.Fatalf("code %d %v", code, err)
	}
	<-out.done
	var want strings.Builder
	for i := 1; i <= 300000; i++ {
		fmt.Fprintf(&want, "%d\n", i)
	}
	want.WriteString("done-go-bye\n")
	if got := out.buf.String(); got != want.String() {
		t.Fatalf("output: %d bytes, want %d (tail %q)", len(got), want.Len(), tail(got))
	}
	waitGone(t, pid, 2*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for heldCount(d.run) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if heldCount(d.run) != 0 {
		t.Fatal("a finished command is still held")
	}
}

func TestResumableTTYKeepsItsTerminal(t *testing.T) {
	d := newTestDirs(t)
	s1 := startAgent(t, d.options())
	h := resumable(sh(`stty size; read x; stty size; echo "got:$x"`))
	h.TTY, h.Rows, h.Cols = true, 24, 80
	e, err := s1.c.Exec(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	out := collect(e.Stdout())
	out.waitFor(t, func(s string) bool { return strings.Contains(s, "24 80") }, 5*time.Second)
	s1.cut()
	waitDetached(t, e)
	if err := e.Resize(40, 100); err != nil { // kept for the re-attach
		t.Fatal(err)
	}
	s2 := startAgent(t, d.options())
	if err := s2.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := e.Stdin().Write([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	got := out.waitFor(t, func(s string) bool { return strings.Contains(s, "got:hi") }, 5*time.Second)
	if !strings.Contains(got, "40 100") || strings.Count(got, "24 80") != 1 {
		t.Fatalf("output %q", got)
	}
	if code, err := e.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
}

// TestResumeIsBoundToItsUserAndPod: a re-attach naming another exec user,
// another pod or an unknown id finds nothing.
func TestResumeIsBoundToItsUserAndPod(t *testing.T) {
	d := newTestDirs(t)
	s1 := startAgent(t, d.options())
	e, err := s1.c.Exec(context.Background(), resumable(sh(`sleep 300`)))
	if err != nil {
		t.Fatal(err)
	}
	s1.cut()
	waitDetached(t, e)
	s2 := startAgent(t, d.options())
	tryRaw := func(h podwire.ExecHeader) podwire.OpenReply {
		conn, err := s2.c.Open(context.Background(), podwire.TagExec, h)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		f, err := podwire.NewFrameReader(conn).Next()
		var reply podwire.OpenReply
		if err != nil || f.Type != podwire.FrameOpen || podwire.DecodeFrame(f, &reply) != nil {
			t.Fatalf("answer %v %v", f.Type, err)
		}
		return reply
	}
	base := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript}, Resume: e.ResumeID()}
	other := base
	other.User = "nobody"
	if r := tryRaw(other); !r.Gone {
		t.Fatalf("another user re-attached: %+v", r)
	}
	other = base
	other.Class = execas.ClassExec
	if r := tryRaw(other); !r.Gone {
		t.Fatalf("another class re-attached: %+v", r)
	}
	other = base
	other.PodUID = "another-pod"
	if r := tryRaw(other); !r.Gone {
		t.Fatalf("another pod re-attached: %+v", r)
	}
	other = base
	other.Resume = strings.Repeat("0", 32)
	if r := tryRaw(other); !r.Gone {
		t.Fatalf("an unknown id re-attached: %+v", r)
	}
	if !alive(e.PID()) {
		t.Fatal("a refused re-attach ended the command")
	}
	// The real one still re-attaches, and closing it aborts the command.
	if err := s2.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	_ = e.Close()
	if code, err := e.Wait(); code != -1 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
	waitGone(t, pid, 2*time.Second)
}

// TestClosingADetachedCommandEndsItOnReattach: Close while detached ends
// the stream at once; the next re-attach aborts the held command.
func TestClosingADetachedCommandEndsItOnReattach(t *testing.T) {
	d := newTestDirs(t)
	s1 := startAgent(t, d.options())
	e, err := s1.c.Exec(context.Background(), resumable(sh(`sleep 300`)))
	if err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	s1.cut()
	waitDetached(t, e)
	_ = e.Close()
	if code, err := e.Wait(); code != -1 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
	if !alive(pid) {
		t.Fatal("ended before any re-attach")
	}
	s2 := startAgent(t, d.options())
	if err := s2.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pid, 3*time.Second)
}

// TestHeldCapEndsTheOldest holds MaxHeld+1 commands: holding the last ends
// the oldest.
func TestHeldCapEndsTheOldest(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.MaxHeld = 2
	var execs []*podwire.Exec
	for i := range 3 {
		s := startAgent(t, o)
		e, err := s.c.Exec(context.Background(), resumable(sh(`sleep 300`)))
		if err != nil {
			t.Fatal(err)
		}
		s.cut()
		waitDetached(t, e)
		deadline := time.Now().Add(5 * time.Second)
		for heldCount(d.run) < min(i+1, 2) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		execs = append(execs, e)
	}
	waitGone(t, execs[0].PID(), 3*time.Second)
	if !alive(execs[1].PID()) || !alive(execs[2].PID()) {
		t.Fatal("a newer held command ended")
	}
	deadline := time.Now().Add(3 * time.Second)
	for heldCount(d.run) != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := heldCount(d.run); n != 2 {
		t.Fatalf("%d held", n)
	}
	s := startAgent(t, o)
	if err := s.c.Reattach(context.Background(), execs[0]); !errors.Is(err, podwire.ErrHeldStreamGone) {
		t.Fatalf("re-attaching the ended one: %v", err)
	}
	if _, err := execs[0].Wait(); !errors.Is(err, podwire.ErrHeldStreamGone) {
		t.Fatalf("its wait: %v", err)
	}
	if err := s.c.Reattach(context.Background(), execs[2]); err != nil {
		t.Fatal(err)
	}
	_ = execs[2].Close()
	execs[1].Fail(nil)
}

func TestHoldGraceEndsAHeldCommand(t *testing.T) {
	d := newTestDirs(t)
	o := d.options()
	o.HoldGrace = 400 * time.Millisecond
	s := startAgent(t, o)
	e, err := s.c.Exec(context.Background(), resumable(sh(`sleep 300`)))
	if err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	s.cut()
	waitDetached(t, e)
	waitGone(t, pid, 3*time.Second)
	s2 := startAgent(t, o)
	if err := s2.c.Reattach(context.Background(), e); !errors.Is(err, podwire.ErrHeldStreamGone) {
		t.Fatalf("got %v", err)
	}
}

// TestExitWhileDetachedIsKept: a command that ends while detached keeps its
// output and status for the re-attach.
func TestExitWhileDetachedIsKept(t *testing.T) {
	d := newTestDirs(t)
	s1 := startAgent(t, d.options())
	e, err := s1.c.Exec(context.Background(), resumable(sh(`sleep 0.3; echo late; exit 5`)))
	if err != nil {
		t.Fatal(err)
	}
	pid := e.PID()
	out := collect(e.Stdout())
	s1.cut()
	waitDetached(t, e)
	waitGone(t, pid, 3*time.Second)
	s2 := startAgent(t, d.options())
	if err := s2.c.Reattach(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if code, err := e.Wait(); code != 5 || err != nil {
		t.Fatalf("code %d %v", code, err)
	}
	<-out.done
	if out.buf.String() != "late\n" {
		t.Fatalf("%q", out.buf.String())
	}
}

// TestHeldCommandsKeepTheirSignals: a held command, run by its shepherd,
// ignores no signal a plain one does not (a shell cannot trap a signal ignored
// on entry, and pipelines print "Broken pipe" with SIGPIPE ignored), for a
// TTY too.
func TestHeldCommandsKeepTheirSignals(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	ignored := func(h podwire.ExecHeader) uint64 {
		t.Helper()
		e, err := s.c.Exec(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(e.Stdout())
		if code, err := e.Wait(); code != 0 || err != nil {
			t.Fatalf("%v: code %d %v", h.Argv, code, err)
		}
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "SigIgn:"))
		mask, err := strconv.ParseUint(line, 16, 64)
		if err != nil {
			t.Fatalf("SigIgn %q: %v", out, err)
		}
		return mask
	}
	probe := sh(`grep SigIgn /proc/$$/status`)
	plain := ignored(probe)
	held := ignored(resumable(probe))
	tty := resumable(probe)
	tty.TTY, tty.Rows, tty.Cols = true, 24, 80
	heldTTY := ignored(tty)
	for name, mask := range map[string]uint64{"held": held, "held tty": heldTTY} {
		if extra := mask &^ plain; extra != 0 {
			t.Errorf("a %s command ignores signals %#x a plain one does not (plain %#x)", name, extra, plain)
		}
	}
}

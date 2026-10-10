package podwire

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// fakeDirect is the agent's end of a direct-mode exec (`pod-agent exec
// --stdio`): it answers the preamble with first, then reads and drops what the
// client sends (or, with stall, never reads again: a transport whose writes
// block); frames written to out go to the client.
func fakeDirect(t *testing.T, first FrameType, reply any, stall bool) (client net.Conn, out *FrameWriter) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	w := NewFrameWriter(a)
	go func() {
		if _, _, err := ReadPreamble(a); err != nil {
			return
		}
		if err := w.WriteJSON(first, reply); err != nil || stall {
			return
		}
		r := NewFrameReader(a)
		for {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	}()
	return b, w
}

// TestExecHeartbeatDetachesOnce: a direct exec whose transport goes silent is
// detached exactly once, though its heartbeat and its reader both see the
// transport go, and stays detached, however long, until it is re-attached
// over a new transport, which then carries the command to its end. A
// transport that stopped reading too (its writes block) is dropped all the
// same.
func TestExecHeartbeatDetachesOnce(t *testing.T) {
	for _, stall := range []bool{false, true} {
		testExecHeartbeatDetachesOnce(t, stall)
	}
}

func testExecHeartbeatDetachesOnce(t *testing.T, stall bool) {
	id := strings.Repeat("ab", 16)
	conn, _ := fakeDirect(t, FrameOpen, OpenReply{ResumeID: id, PID: 7}, stall)
	h := ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh"}, TTY: true, Rows: 24, Cols: 80}, Resumable: true}
	opts := ExecOptions{Heartbeat: true, PingInterval: 10 * time.Millisecond, Silence: 60 * time.Millisecond}
	e, err := OpenExec(context.Background(), conn, h, opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitFor(e.Detached):
	case <-time.After(3 * time.Second):
		t.Fatalf("stall %v: a silent transport was not dropped", stall)
	}
	// Detached for many silence deadlines: still waiting to be re-attached.
	time.Sleep(20 * opts.Silence)
	select {
	case <-e.Done():
		t.Fatalf("a detached command ended: %v", e.err)
	default:
	}

	conn2, out := fakeDirect(t, FrameResume, ResumeReply{PID: 7}, false)
	if err := e.ReattachOver(context.Background(), conn2); err != nil {
		t.Fatal(err)
	}
	if err := out.Write(FrameData, []byte("back")); err != nil {
		t.Fatal(err)
	}
	if err := out.WriteJSON(FrameExit, ExitStatus{Code: 3}); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(e.Stdout())
	if code, err := e.Wait(); code != 3 || err != nil || string(got) != "back" {
		t.Fatalf("after re-attach: %q, code %d, %v", got, code, err)
	}
}

// waitFor is closed once ch() returns a channel that is closed.
func waitFor(ch func() <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if c := ch(); c != nil {
				<-c
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return done
}

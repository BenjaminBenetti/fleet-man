//go:build linux

package podagent

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// TestProcEndedBeforeAttachKeepsItsStatus: a command that ends, output and
// all, before its stream attaches (the agent still writing OPEN, or a
// shepherd waiting for its agent) still sends its output and status down
// that stream; it is not orphaned.
func TestProcEndedBeforeAttachKeepsItsStatus(t *testing.T) {
	d := newTestDirs(t)
	o := d.options().withDefaults()
	plan, err := loadHost(o, newStderrLogger(testWriter{t})).Plan(context.Background(), sh(`echo out; exit 3`).Request)
	if err != nil {
		t.Fatal(err)
	}
	for _, resumable := range []bool{false, true} {
		p, err := startProc(plan, resumable)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mu.Lock()
			exited := p.exited
			p.mu.Unlock()
			if exited {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the command did not end")
			}
			time.Sleep(5 * time.Millisecond)
		}
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		att := &attachment{r: podwire.NewFrameReader(inR), w: podwire.NewFrameWriter(outW), c: outW}
		if err := p.attach(att, 0, 0, nil); err != nil {
			t.Fatalf("resumable %v: %v", resumable, err)
		}
		r := podwire.NewFrameReader(outR)
		var out []byte
		var st podwire.ExitStatus
		for {
			f, err := r.Next()
			if err != nil {
				t.Fatalf("resumable %v: no EXIT: %v", resumable, err)
			}
			if f.Type == podwire.FrameData {
				out = append(out, f.Payload...)
			}
			if f.Type == podwire.FrameExit {
				_ = podwire.DecodeFrame(f, &st)
				break
			}
		}
		if string(out) != "out\n" || st.Code != 3 {
			t.Fatalf("resumable %v: %q, %+v", resumable, out, st)
		}
		if resumable {
			// The client's EXIT: it has the status.
			go func() { _ = podwire.NewFrameWriter(inW).Write(podwire.FrameExit, nil) }()
		}
		select {
		case <-p.finishedCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("resumable %v: not finished", resumable)
		}
		_ = inW.Close()
		_ = outR.Close()
	}
}

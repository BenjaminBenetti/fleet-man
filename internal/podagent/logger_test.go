package podagent

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// TestLoggerRoutes: records wait for the control stream and go out on it;
// without one (it is gone, or there never is one, as in a direct exec) they
// go to stderr, none lost.
func TestLoggerRoutes(t *testing.T) {
	var stderr bytes.Buffer
	l := newLogger(&stderr)
	l.Warn("before", "k", "v")

	pr, pw := io.Pipe()
	got := make(chan []string, 1)
	go func() {
		var msgs []string
		r := podwire.NewFrameReader(pr)
		for range 2 {
			f, err := r.Next()
			if err != nil {
				break
			}
			var m podwire.ControlMessage
			if podwire.DecodeFrame(f, &m) == nil && m.Log != nil {
				msgs = append(msgs, m.Log.Msg)
			}
		}
		got <- msgs
	}()
	l.attach(podwire.NewFrameWriter(pw))
	l.Error("during")
	if msgs := <-got; strings.Join(msgs, ",") != "before,during" {
		t.Fatalf("on the control stream: %v", msgs)
	}
	l.detach()
	l.Info("after")
	if stderr.String() != "pod-agent: info: after\n" {
		t.Fatalf("stderr %q", stderr.String())
	}

	stderr.Reset()
	l = newLogger(&stderr)
	l.Warn("queued")
	l.detach() // no control stream ever came
	if stderr.String() != "pod-agent: warn: queued\n" {
		t.Fatalf("stderr %q", stderr.String())
	}

	stderr.Reset()
	newStderrLogger(&stderr).Error("direct", "path", "/x")
	if stderr.String() != "pod-agent: error: direct path=\"/x\"\n" {
		t.Fatalf("stderr %q", stderr.String())
	}
}

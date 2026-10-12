package devcli

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// streamingExecer writes a command's stdout and stderr from a goroutine
// each, as the pod agent's stream does, then exits with code.
type streamingExecer struct{ code int }

func (e streamingExecer) Exec(_ context.Context, _ kubeshim.ExecRequest, stdio kubeshim.ExecStdio) (int, error) {
	var wg sync.WaitGroup
	for _, w := range []struct {
		out  func([]byte) (int, error)
		line string
	}{{stdio.Stdout.Write, "out\n"}, {stdio.Stderr.Write, "err\n"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, _ = w.out([]byte(w.line))
			}
		}()
	}
	wg.Wait()
	return e.code, nil
}

// execOK takes stdout and stderr written at once into one buffer without a
// data race (go test -race), and a failure keeps both in its error.
func TestExecOKTakesStdoutAndStderrAtOnce(t *testing.T) {
	if err := execOK(context.Background(), streamingExecer{}, kubeshim.ExecRequest{}); err != nil {
		t.Fatal(err)
	}
	err := execOK(context.Background(), streamingExecer{code: 3}, kubeshim.ExecRequest{})
	if err == nil {
		t.Fatal("exit 3 passed")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "exit 3: ") || strings.Count(msg, "out") != 200 || strings.Count(msg, "err") != 200 {
		t.Fatalf("error %.80q... has %d out and %d err lines; want 200 of each", msg, strings.Count(msg, "out"), strings.Count(msg, "err"))
	}
}

package devcli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// hangingExecer runs the data folder's execs, and hangs on the hook: until
// ctx ends, or (deaf) until the test ends whatever ctx does, as an exec
// whose pod lost track of it would.
type hangingExecer struct {
	recordingExecer
	deaf    bool
	release chan struct{}
}

func (h *hangingExecer) Exec(ctx context.Context, req kubeshim.ExecRequest, stdio kubeshim.ExecStdio) (int, error) {
	if !strings.Contains(strings.Join(req.Argv, " "), "run hook") {
		return h.recordingExecer.Exec(ctx, req, stdio)
	}
	if h.deaf {
		<-h.release
		return -1, errors.New("released")
	}
	<-ctx.Done()
	return -1, ctx.Err()
}

// TestSetUpTimesOutNamingWhatRuns: a set-up whose hook never ends is stopped
// at its context's deadline, and its failure names the exec still running in
// the pod: also when that exec does not end once aborted, which then holds
// the set-up only for the serve grace.
func TestSetUpTimesOutNamingWhatRuns(t *testing.T) {
	defer func(d time.Duration) { setUpServeGrace = d }(setUpServeGrace)
	setUpServeGrace = 200 * time.Millisecond
	for _, deaf := range []bool{false, true} {
		f := newFakeTools(t)
		ex := &hangingExecer{deaf: deaf, release: make(chan struct{})}
		t.Cleanup(func() { close(ex.release) })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		start := time.Now()
		_, err := SetUp(ctx, SetUpRequest{Tools: f.Tools, Container: setUpContainer(), WorkspaceFolder: "/workspaces/app", Execer: ex})
		cancel()
		took := time.Since(start)
		var te *TimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("deaf %v: a set-up past its deadline failed with %v, want a TimeoutError", deaf, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("deaf %v: the failure does not carry the deadline: %v", deaf, err)
		}
		if len(te.Running) != 1 || !strings.HasPrefix(te.Running[0], `"/bin/sh -c run hook" as vscode for `) {
			t.Errorf("deaf %v: still running %q, want the hook as vscode", deaf, te.Running)
		}
		if !strings.Contains(err.Error(), "still running in the pod: \"/bin/sh -c run hook\" as vscode") {
			t.Errorf("deaf %v: the failure does not name the hook: %v", deaf, err)
		}
		if took > 15*time.Second {
			t.Errorf("deaf %v: the set-up took %s past a 2s deadline", deaf, took)
		}
	}
}

// TestLiveExecsRunningAt: an exec counts as running at t when it started by
// t and had not ended by then; the oldest comes first.
func TestLiveExecsRunningAt(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	l := newLiveExecs(nil)
	l.now = func() time.Time { return now }
	add := func(argv string, start, end time.Duration) {
		e := &trackedExec{req: kubeshim.ExecRequest{User: "vscode", Argv: strings.Fields(argv), Stdin: true}, start: now.Add(start)}
		if end != 0 {
			e.end = now.Add(end)
		}
		l.execs = append(l.execs, e)
	}
	add("/bin/sh", -60*time.Minute, 0)
	add("ended before", -10*time.Minute, -9*time.Minute)
	add("bash -lic probe", -59*time.Minute, 0)
	add("ended after", -2*time.Minute, time.Minute)
	add("started after", time.Minute, 0)
	got := l.runningAt(now)
	want := []string{`"bash -lic probe" as vscode for 59m0s`, `"ended after" as vscode for 2m0s`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("running %q, want %q", got, want)
	}
	servers := newLiveExecs(nil)
	servers.execs = []*trackedExec{{req: kubeshim.ExecRequest{User: "root", Argv: []string{"/bin/sh"}, Stdin: true}, start: now.Add(-time.Minute)}}
	if got := servers.runningAt(now); len(got) != 1 || got[0] != `"/bin/sh" as root for 1m0s` {
		t.Errorf("only the CLI's shell server running: %q", got)
	}
	long := describeExec(kubeshim.ExecRequest{Argv: []string{strings.Repeat("x", 200)}}, time.Second)
	if !strings.Contains(long, "…") || !strings.Contains(long, "as the container's user") {
		t.Errorf("a long command as the container's user: %s", long)
	}
}

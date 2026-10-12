package devcli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/redact"
)

// TimeoutError is a set-up whose context's deadline ran out: the CLI was
// stopped, and Running is what it still had running in the pod at the
// deadline (the hook, or the userEnvProbe's login shell, that never ended).
type TimeoutError struct {
	// Running is each exec as "<command>" as <user> for <how long>, oldest
	// first; empty when the CLI itself was all that still ran.
	Running []string
	// Err is the CLI run's error.
	Err error
}

func (e *TimeoutError) Error() string {
	if len(e.Running) == 0 {
		return e.Err.Error()
	}
	return e.Err.Error() + "\nstill running in the pod: " + strings.Join(e.Running, "; ")
}

// Unwrap is the run's error and context.DeadlineExceeded: the CLI's error
// keeps only its words.
func (e *TimeoutError) Unwrap() []error { return []error{e.Err, context.DeadlineExceeded} }

// maxTrackedExecs bounds the execs liveExecs remembers: a set-up runs a few
// dozen.
const maxTrackedExecs = 256

// maxExecDescription clips one exec's command in a description.
const maxExecDescription = 120

// liveExecs is an Execer that remembers when each of the execs it runs
// started and ended, to say which ones were still running when a set-up ran
// out of time (the CLI and its shims are gone by the time the error is made,
// so the execs they asked for have ended too).
type liveExecs struct {
	ex kubeshim.Execer
	// now is time.Now (tests stand in).
	now func() time.Time

	mu    sync.Mutex
	execs []*trackedExec
}

type trackedExec struct {
	req        kubeshim.ExecRequest
	start, end time.Time
}

func newLiveExecs(ex kubeshim.Execer) *liveExecs {
	return &liveExecs{ex: ex, now: time.Now}
}

// Exec runs req on the wrapped Execer, remembering it.
func (l *liveExecs) Exec(ctx context.Context, req kubeshim.ExecRequest, stdio kubeshim.ExecStdio) (int, error) {
	e := &trackedExec{req: req, start: l.now()}
	l.mu.Lock()
	if len(l.execs) >= maxTrackedExecs {
		l.dropEndedLocked()
	}
	if len(l.execs) < maxTrackedExecs {
		l.execs = append(l.execs, e)
	}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		e.end = l.now()
		l.mu.Unlock()
	}()
	return l.ex.Exec(ctx, req, stdio)
}

func (l *liveExecs) dropEndedLocked() {
	kept := l.execs[:0]
	for _, e := range l.execs {
		if e.end.IsZero() {
			kept = append(kept, e)
		}
	}
	l.execs = kept
}

// runningAt describes the execs that had started and not ended at t, oldest
// first. The CLI's shell servers (a shell it keeps open to feed commands
// on stdin) run all along: they are named only when nothing else ran.
func (l *liveExecs) runningAt(t time.Time) []string {
	l.mu.Lock()
	var running, servers []*trackedExec
	for _, e := range l.execs {
		if !e.start.After(t) && (e.end.IsZero() || e.end.After(t)) {
			if shellServer(e.req) {
				servers = append(servers, e)
			} else {
				running = append(running, e)
			}
		}
	}
	l.mu.Unlock()
	if len(running) == 0 {
		running = servers
	}
	sort.SliceStable(running, func(i, j int) bool { return running[i].start.Before(running[j].start) })
	out := make([]string, 0, len(running))
	for _, e := range running {
		out = append(out, describeExec(e.req, t.Sub(e.start)))
	}
	return out
}

// shellServer reports an exec that is one of the CLI's shell servers: a
// bare shell reading its commands from stdin.
func shellServer(req kubeshim.ExecRequest) bool {
	return req.Stdin && len(req.Argv) == 1
}

// describeExec is one exec as a set-up's failure names it: its command,
// clipped and with environment values hidden, as whom, for how long.
func describeExec(req kubeshim.ExecRequest, running time.Duration) string {
	cmd := redact.EnvText(strings.Join(req.Argv, " "))
	if r := []rune(cmd); len(r) > maxExecDescription {
		cmd = string(r[:maxExecDescription]) + "…"
	}
	user := req.User
	if user == "" {
		user = "the container's user"
	}
	return fmt.Sprintf("%q as %s for %s", cmd, user, running.Round(time.Second))
}

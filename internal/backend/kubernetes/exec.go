//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// exec.go runs commands in an instance's pod for fleetd itself: the streams
// the daemon holds (backend.StreamExecer: the Exec RPC's shells, a folder's
// tar) and the commands the kube-exec shim's daemon mode asks for (PodExec).
// Each runs on the pod's agent session, whose agent verified the pod before
// it was connected. Shells and attaches are resumable: when the session
// drops (an API server cut, a laptop asleep) the agent holds the command and
// the pool re-attaches it to the pod's next session; when the pod itself was
// replaced, the command is gone with it, and its stream ends with
// podwire.ExitHeldStreamGone. A pod whose agent cannot be reached runs the
// command through a plain kube exec of `fleet exec-as` instead (no TTY, not
// resumable).

// PodCommand is one command a PodExec runs in the instance's pod.
type PodCommand struct {
	// Class is whom it runs as (execas.ClassExec when empty); User and Cwd
	// name them explicitly where the class takes them.
	Class     execas.Class
	User, Cwd string
	Argv      []string
	Env       map[string]string
	// TTY runs it on a terminal of Rows x Cols.
	TTY        bool
	Rows, Cols uint16
	// Bulk runs it on the bulk session, not resumable: a folder's tar.
	Bulk bool
	// Audio runs it on the agent session, not resumable (an audio stream
	// fails closed: a lost transport ends it), after a lazy restage of the
	// pod's fleet binary (an audio open is one of its moments).
	Audio bool
}

// execClass is the execas class of a backend exec class.
func execClass(c backend.ExecClass) execas.Class {
	if c == backend.ExecClassScript {
		return execas.ClassScript
	}
	return execas.ClassExec
}

// OpenExec starts req's command in the instance's pod (backend.StreamExecer).
// ctx bounds the start: a wait for the pod's agent when none is connected.
func (b *Backend) OpenExec(ctx context.Context, req backend.ExecRequest) (backend.ExecStream, error) {
	return b.OpenPodExec(ctx, PodCommand{Class: execClass(req.Class), Argv: req.Argv, Env: req.Env, TTY: req.TTY, Rows: req.Rows, Cols: req.Cols, Bulk: req.Bulk, Audio: req.Audio})
}

// OpenPodExec starts cmd in the instance's pod. ctx bounds the start.
func (b *Backend) OpenPodExec(ctx context.Context, cmd PodCommand) (backend.ExecStream, error) {
	if len(cmd.Argv) == 0 {
		return nil, errors.New("exec: no command")
	}
	if cmd.Class == "" {
		cmd.Class = execas.ClassExec
	}
	req := execas.Request{Class: cmd.Class, Argv: cmd.Argv, User: cmd.User, Cwd: cmd.Cwd, Env: cmd.Env, TTY: cmd.TTY}
	if cmd.TTY {
		req.Rows, req.Cols = cmd.Rows, cmd.Cols
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	s, err := b.agent(ctx, cmd.Audio)
	if err != nil {
		if !fallbackable(err) || cmd.TTY {
			return nil, err
		}
		return b.fallbackExec(req)
	}
	header := podwire.ExecHeader{Request: req}
	if cmd.Bulk {
		bulk, err := s.Bulk(ctx)
		if err != nil {
			return nil, fmt.Errorf("the pod's bulk session: %w", err)
		}
		e, err := bulk.Exec(ctx, header)
		if err != nil {
			return nil, startError(err)
		}
		return podStream{e}, nil
	}
	header.Resumable = !cmd.Audio
	e, err := s.Exec(ctx, header)
	if err != nil {
		return nil, startError(err)
	}
	return podStream{e}, nil
}

// fallbackable reports whether a command may run without the pod's agent
// after err kept it from connecting (its bootstrap failed or timed out): not
// when the pod failed verification (nothing goes to it), is not up, the
// connection does not load, or the caller gave up.
func fallbackable(err error) bool {
	var tampered *TamperedError
	var timedOut *DeadlineError
	switch {
	case errors.As(err, &tampered), errors.Is(err, ErrUnverifiable), errors.Is(err, ErrPodNotReady),
		errors.Is(err, ErrClusterNotFound), errors.Is(err, errUnbound), errors.Is(err, errNoInstanceRecord):
		return false
	case errors.As(err, &timedOut):
		// The bootstrap's own deadline (it wraps the context error it ended
		// with): the agent did not connect in time, the fallback's case.
		return true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

// startError is a command the agent did not start: its StartError (with
// docker's 125/126/127) as is, anything else wrapped.
func startError(err error) error {
	var se *podwire.StartError
	if errors.As(err, &se) {
		return se
	}
	return fmt.Errorf("start exec: %w", err)
}

// ErrConnectionLost is how a command whose pod was replaced ends: it ran in
// the old pod, so it ended with it (podwire.ExitHeldStreamGone).
var ErrConnectionLost = errors.New("connection lost: the instance's pod was replaced, and the command ended with it")

// podStream is an agent command as a backend.ExecStream: a command whose held
// stream is gone exits podwire.ExitHeldStreamGone.
type podStream struct{ *podwire.Exec }

func (s podStream) Wait() (int, error) {
	code, err := s.Exec.Wait()
	if errors.Is(err, podwire.ErrHeldStreamGone) {
		return podwire.ExitHeldStreamGone, ErrConnectionLost
	}
	return code, err
}

// fallbackExec runs req without the pod's agent: one paced kube exec of
// `fleet exec-as` (Pool.FallbackExec), which verifies the pod first.
func (b *Backend) fallbackExec(req execas.Request) (backend.ExecStream, error) {
	t, err := b.PodTarget()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	f := &fallbackStream{stdin: &closeOnce{w: inW}, stdout: outR, stderr: errR, cancel: cancel, done: make(chan struct{})}
	go func() {
		code, err := b.p().FallbackExec(ctx, t, req, kubeexec.Stdio{Stdin: inR, Stdout: outW, Stderr: errW})
		_ = inR.CloseWithError(errStdinEnded)
		_ = outW.Close()
		_ = errW.Close()
		f.code, f.err = code, err
		if err != nil {
			f.code = -1
		}
		close(f.done)
	}()
	return f, nil
}

var errStdinEnded = errors.New("the command has ended")

// fallbackStream is a fallback exec as a backend.ExecStream.
type fallbackStream struct {
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
	cancel context.CancelFunc
	done   chan struct{}
	code   int
	err    error
}

func (f *fallbackStream) Stdin() io.WriteCloser { return f.stdin }
func (f *fallbackStream) Stdout() io.Reader     { return f.stdout }
func (f *fallbackStream) Stderr() io.Reader     { return f.stderr }
func (f *fallbackStream) Resize(uint16, uint16) error {
	return nil
}

func (f *fallbackStream) Wait() (int, error) {
	<-f.done
	f.cancel()
	return f.code, f.err
}

func (f *fallbackStream) Close() error {
	f.cancel()
	return nil
}

// closeOnce closes a pipe writer once; later closes do nothing.
type closeOnce struct {
	w    *io.PipeWriter
	once sync.Once
}

func (c *closeOnce) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *closeOnce) Close() error {
	c.once.Do(func() { _ = c.w.Close() })
	return nil
}

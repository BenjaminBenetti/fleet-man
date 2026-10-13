//go:build !nok8s

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// kube_exec_direct.go is `fleet kube-exec`'s direct mode and `logs`: a
// terminal attach (and the raw fallback) straight to the API server with the
// transport fleetd itself uses, re-attached after a drop so a pane outlives
// an apiserver rollout, a network blip or a fleetd restart; and the saved +
// live container logs. A cluster the shim cannot reach itself fails with
// kubernetes:unreachable (fleetd's answers carry their own codes). The
// cluster is reached only through internal/kubeexec (the client-go leaf; the
// transport is opaque here), so the -tags nok8s pod build gets a stub
// (kube_exec_nok8s.go) — the pod never runs the shim.

// redialBackoff paces re-dials after a drop, and an attach's asks while
// fleetd cannot vouch for its pod yet.
var redialBackoff = []time.Duration{0, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// attachWait bounds how long an attach waits for fleetd to vouch for the pod
// that is there now (kubeExecOpen). A var for tests.
var attachWait = 2 * time.Minute

// runKubeExecDirect attaches argv to the pod over the API server. On a TTY the
// local terminal is put into raw mode once the command starts, and resizes
// are forwarded; raw mode (no fleet binary in the pod) runs the command with
// the pod's own shell and does not resume.
func runKubeExecDirect(ctx context.Context, f kubeExecFlags, argv []string, tty bool) (int, error) {
	if !f.raw {
		return runKubeExecAgent(ctx, f, argv, tty)
	}
	t, _, err := kubeExecTransport(ctx, f, f.pod, false)
	if err != nil {
		return -1, err
	}
	defer kubeExecTerminal(tty)()
	return runKubeExecRaw(ctx, t, f, argv, tty)
}

// runKubeExecAgent attaches through `pod-agent exec --stdio`, so the command
// is held and re-attached after a dropped connection.
func runKubeExecAgent(ctx context.Context, f kubeExecFlags, argv []string, tty bool) (int, error) {
	rows, cols := kubeExecSize()
	header := podwire.ExecHeader{
		Request:   execas.Request{Class: execas.Class(f.class), User: f.user, Cwd: f.cwd, Argv: argv, TTY: tty, Rows: rows, Cols: cols},
		Resumable: true,
		ProtoMin:  podwire.ProtoMin,
		ProtoMax:  podwire.ProtoMax,
	}
	var t *kubeexec.Transport
	var stream io.ReadWriteCloser
	// agentErr is the agent's answer to the last open; nil when fleetd's
	// answer ended it before the agent was reached.
	var agentErr error
	e, err := kubeExecOpen(ctx, func(ctx context.Context) (*podwire.Exec, error) {
		agentErr = nil
		fresh, podUID, err := kubeExecTransport(ctx, f, f.pod, false)
		if err != nil {
			return nil, err
		}
		t, stream = fresh, fresh.Stream(ctx, podAgentStdioReq(f))
		h := header
		h.PodUID = podUID
		e, err := podwire.OpenExec(ctx, stream, h, podwire.ExecOptions{Heartbeat: true})
		if err != nil {
			_ = stream.Close()
			agentErr = err
		}
		return e, err
	})
	switch {
	case err == nil:
	case agentErr == nil:
		return -1, err
	case isAgentMissing(err):
		// The pod has no fleet binary: run with its own shell instead.
		defer kubeExecTerminal(tty)()
		return runKubeExecRaw(ctx, t, f, argv, tty)
	default:
		var se *podwire.StartError
		if errors.As(err, &se) {
			fmt.Fprintln(os.Stderr, se.Message)
			return se.Code, nil
		}
		return -1, kubeerr.Unreached(err)
	}
	// Raw only now: until the command started, the terminal stays the
	// user's (a wait for the pod can be interrupted).
	defer kubeExecTerminal(tty)()
	go bridgeStdin(e)
	if tty {
		go kubeExecResizes(e)
	}
	drained := copyOutput(e.Stdout(), e.Stderr(), os.Stdout, os.Stderr)
	go kubeExecResume(ctx, e, func(ctx context.Context, attempt int) (io.ReadWriteCloser, error) {
		// The first re-dial of a drop takes fleetd's cached credential (it
		// renews one near expiry itself); a re-dial after a failed one asks
		// for a fresh one, in case the API server refused the cached.
		fresh, _, err := kubeExecTransport(ctx, f, f.pod, attempt > 0)
		if err != nil {
			// Refused, or not now (the pod is still being replaced): never
			// an unverified re-attach.
			return nil, err
		}
		t = fresh
		return t.Stream(ctx, podAgentStdioReq(f)), nil
	})
	code, werr := e.Wait()
	// The caller exits as soon as this returns: what the command printed
	// last (a tmux detach's terminal reset, a one-shot's tail) must reach
	// the terminal first.
	drained()
	_ = stream.Close()
	return attachExit(code, werr)
}

// kubeExecOpen starts an attach's command with open, one ask of fleetd for
// the pod (a fresh verification, and the uid it vouches for) and the start of
// the command in the pod the API server reaches by name. The two can disagree
// for a while after the pod was replaced: fleetd's view of the cluster
// catches up on its own (its watch; after an API server outage, as much as a
// minute later), and until it does it vouches for the pod that is gone (whose
// uid the new pod's agent refuses, podwire.RefusedOtherPod) or sees the new
// pod not running yet (errKubeExecNotNow). A replaced pod's pane rejoins
// through exactly this attach, so it asks again (redialBackoff) for up to
// attachWait; fleetd refusing the pod, and any other failure, ends it at once.
// Each ask is verified afresh, so the command only ever starts in a pod
// fleetd vouched for, by its uid.
func kubeExecOpen(ctx context.Context, open func(context.Context) (*podwire.Exec, error)) (*podwire.Exec, error) {
	deadline := time.Now().Add(attachWait)
	for attempt := 1; ; attempt++ {
		e, err := open(ctx)
		if err == nil || !attachAgain(err) {
			return e, err
		}
		wait := redialBackoff[min(attempt, len(redialBackoff)-1)]
		if time.Now().Add(wait).After(deadline) {
			return nil, err
		}
		if attempt == 1 {
			fmt.Fprintf(os.Stderr, "fleet kube-exec: waiting for fleetd to vouch for the instance's pod (%v)\n", err)
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

// attachAgain reports whether an attach's start failed only because fleetd
// cannot vouch for the pod that is there yet: it answered not now, or the
// pod's agent refused the uid fleetd vouched for (it replaced that pod).
func attachAgain(err error) bool {
	var refused *podwire.StreamRefusedError
	return errors.Is(err, errKubeExecNotNow) || errors.As(err, &refused) && refused.Reason == podwire.RefusedOtherPod
}

// kubeExecTerminal puts a terminal attach's stdin into raw mode; the func it
// returns restores it.
func kubeExecTerminal(tty bool) func() {
	if tty {
		if restore := kubeExecRawTerminal(); restore != nil {
			return restore
		}
	}
	return func() {}
}

// attachExit is the shim's exit for an attach that ended with code and werr:
// 75 when the pod was replaced (its wrapper rejoins a fresh session), 1 when
// the connection was lost for good, else the command's own code.
func attachExit(code int, werr error) (int, error) {
	switch {
	case werr == nil:
		return code, nil
	case errors.Is(werr, podwire.ErrHeldStreamGone):
		// The pod was replaced; the shell ended with it. Exit 75 so the
		// pane's wrapper rejoins a fresh session.
		fmt.Fprintln(os.Stderr, "connection lost: the instance's pod was replaced")
		return podwire.ExitHeldStreamGone, nil
	case errors.Is(werr, podwire.ErrConnectionLost):
		fmt.Fprintln(os.Stderr, "connection lost")
		return 1, nil
	}
	fmt.Fprintf(os.Stderr, "connection lost: %v\n", werr)
	return 1, nil
}

// copyOutput copies a command's stdout and stderr to the terminal's and
// returns a func that waits until both are drained (the command's streams end
// when it does).
func copyOutput(stdout, stderr io.Reader, toOut, toErr io.Writer) (drained func()) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(toOut, stdout) }()
	go func() { defer wg.Done(); _, _ = io.Copy(toErr, stderr) }()
	return wg.Wait
}

// kubeExecResume re-attaches e whenever it loses its transport, until it
// ends: redial is a new API server exec of the pod's agent (attempt counts the
// drop's re-dials from 0), on a credential from fleetd (or the kubeconfig
// while fleetd is down). Re-dials back off and a re-dial that fails is tried
// again; a pod
// that replaced e's (the held command is gone) ends e with
// ErrHeldStreamGone, and fleetd refusing the pod (errKubeExecRefused) ends it
// with that refusal.
func kubeExecResume(ctx context.Context, e *podwire.Exec, redial func(ctx context.Context, attempt int) (io.ReadWriteCloser, error)) {
	for {
		// DetachSignal, not Detached: Detached is nil while e is attached,
		// and a select on it would never see the drop.
		select {
		case <-e.Done():
			return
		case <-e.DetachSignal():
		}
		for attempt := 0; ; attempt++ {
			select {
			case <-e.Done():
				return
			case <-time.After(redialBackoff[min(attempt, len(redialBackoff)-1)]):
			}
			stream, err := redial(ctx, attempt)
			if errors.Is(err, errKubeExecRefused) {
				// fleetd no longer vouches for the pod (it was substituted):
				// the command's input must not go to it.
				e.Fail(err)
				return
			}
			if err != nil {
				continue // not now (the pod is starting, the API is away)
			}
			err = e.ReattachOver(ctx, stream)
			if err == nil {
				break
			}
			_ = stream.Close()
			if errors.Is(err, podwire.ErrHeldStreamGone) {
				return // Wait sees it and exits 75.
			}
		}
	}
}

// runKubeExecRaw runs argv with the pod's own shell: for a pod with no fleet
// binary (very early boot). No agent, so no resume.
func runKubeExecRaw(ctx context.Context, t *kubeexec.Transport, f kubeExecFlags, argv []string, tty bool) (int, error) {
	req := kubeexec.ExecRequest{
		Namespace: f.namespace, Pod: f.pod, Container: f.container,
		Command: argv, Stdin: true, TTY: tty,
	}
	stdio := kubeexec.Stdio{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if tty {
		stdio.Stderr = nil
		stdio.Resize = kubeExecRawResizes(ctx)
	}
	err := t.Exec(ctx, req, stdio)
	var exit *kubeexec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.Code, nil
	}
	return -1, kubeerr.Unreached(err)
}

// runKubeExecLogs prints the saved container.log (every earlier run) then the
// running pod's live log (when it runs); -f follows the live part. A stopped
// instance shows the saved log alone.
func runKubeExecLogs(ctx context.Context, f kubeExecLogsFlags) error {
	if f.fleet != "" && f.instance != "" {
		if saved, err := os.Open(fleetpaths.ContainerLogPath(f.fleet, f.instance)); err == nil {
			_, _ = io.Copy(os.Stdout, saved)
			_ = saved.Close()
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("the saved container log: %w", err)
		}
	}
	t, _, err := kubeExecTransport(ctx, f.kubeExecFlags, "", false)
	if err != nil {
		// Without a cluster the saved log is all there is (a stopped
		// instance on an unreachable cluster).
		return nil
	}
	err = t.PodLogs(ctx, kubeexec.PodLogsRequest{Namespace: f.namespace, Pod: f.pod, Container: f.container, Follow: f.follow}, os.Stdout)
	if err != nil && !isPodNotRunning(err) {
		return kubeerr.Unreached(err)
	}
	return nil
}

// isPodNotRunning reports whether err is the live log failing because the pod
// is gone or not running (a stopped instance): the saved log stands alone.
func isPodNotRunning(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"not found", "containercreating", "is not available", "waiting to start", "podinitializing", "terminated"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// errKubeExecRefused is fleetd refusing an attach for good: the pod is not
// one of its instances', or did not verify. The shim never goes around it
// with the kubeconfig.
var errKubeExecRefused = errors.New("fleetd refused the attach")

// kubeExecTransport is the transport an attach reaches the pod with: fleetd's
// own (IssueKubeExecCredential, which verifies pod, when named, and returns
// its uid), or, only when fleetd does not answer (down, or a build without
// Kubernetes support), the connection's kubeconfig loaded here. A refusal
// from fleetd is final (errKubeExecRefused); its other errors are "not now".
func kubeExecTransport(ctx context.Context, f kubeExecFlags, pod string, refresh bool) (*kubeexec.Transport, string, error) {
	if conn, err := fleetclient.DialSocket(ctx, f.socket); err == nil {
		defer conn.Close()
		reply, err := conn.Service().IssueKubeExecCredential(ctx, &fleetgrpc.IssueKubeExecCredentialRequest{
			ClusterId: f.cluster, Pod: pod, Refresh: refresh,
		})
		if err == nil {
			t, err := kubeexec.TransportFor(protoconv.KubeExecCredentialFromProto(reply.GetCredential()))
			if err != nil {
				return nil, "", fmt.Errorf("fleetd's credential: %w", err)
			}
			return t, reply.GetPodUid(), nil
		}
		switch {
		case fleetdRefused(err):
			return nil, "", fmt.Errorf("%w: %v", errKubeExecRefused, status.Convert(err).Message())
		case !fleetdUnavailable(err):
			// fleetd answered, but not now (the pod is not running yet, the
			// API server is away): the caller may try again, never around it.
			return nil, "", fleetdAnswer(err)
		}
	}
	// fleetd will not issue (down, or no Kubernetes support): load the
	// kubeconfig ourselves, as the shim's self-contained argv carries it.
	conn, err := kubeexec.Load(fleet.KubeCluster{ID: f.cluster, Context: f.context, KubeconfigPaths: f.kubeconfig, Namespace: f.namespace}, kubeexec.Options{Environ: os.Environ()})
	if err != nil {
		return nil, "", fmt.Errorf("reaching the cluster: %w", err)
	}
	return conn.Transport(), "", nil
}

// errKubeExecNotNow is fleetd unable to vouch for the pod now: it is not
// running yet as fleetd sees it, or fleetd could not check it (the API server
// away or slow).
var errKubeExecNotNow = errors.New("fleetd cannot vouch for the pod now")

// fleetdAnswer is fleetd's answer err to an issue that is neither a refusal
// nor fleetd being away, with fleetd's message: errKubeExecNotNow when it
// could not vouch for the pod now (FailedPrecondition, DeadlineExceeded).
func fleetdAnswer(err error) error {
	msg := status.Convert(err).Message()
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.DeadlineExceeded:
		return &notNowError{msg}
	}
	return errors.New(msg)
}

// notNowError is errKubeExecNotNow with fleetd's message as is (it leads
// with its kubernetes: code).
type notNowError struct{ msg string }

func (e *notNowError) Error() string { return e.msg }

func (e *notNowError) Is(target error) bool { return target == errKubeExecNotNow }

// fleetdUnavailable reports whether err is fleetd not answering an issue (the
// daemon went away mid-call, or is a build without the RPC), as opposed to it
// answering.
func fleetdUnavailable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Unimplemented:
		return true
	}
	return false
}

// fleetdRefused reports whether err is fleetd refusing the pod for good: it
// is not one of its instances' (NotFound), or it did not verify
// (PermissionDenied).
func fleetdRefused(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.PermissionDenied, codes.InvalidArgument:
		return true
	}
	return false
}

// podAgentStdioReq is the kube exec that runs the pod's fleet binary as the
// direct-exec agent.
func podAgentStdioReq(f kubeExecFlags) kubeexec.ExecRequest {
	return kubeexec.ExecRequest{Namespace: f.namespace, Pod: f.pod, Container: f.container, Command: []string{podseed.CurrentBinary, "pod-agent", "exec", "--stdio"}}
}

// isAgentMissing reports whether err is the agent exec never starting because
// the pod has no fleet binary: the runtime's own 126/127 for the kube exec of
// a missing program (or the agent's, for a binary that cannot run).
func isAgentMissing(err error) bool {
	var se *podwire.StartError
	if errors.As(err, &se) {
		return se.Code == execas.ExitNotFound || se.Code == execas.ExitCannotRun
	}
	var exit *kubeexec.ExitError
	if errors.As(err, &exit) {
		return exit.Code == execas.ExitNotFound || exit.Code == execas.ExitCannotRun
	}
	return false
}

// bridgeStdin copies the terminal's input to the command until it ends.
func bridgeStdin(e *podwire.Exec) {
	_, _ = io.Copy(e.Stdin(), os.Stdin)
	_ = e.Stdin().Close()
}

// kubeExecResizes forwards the terminal's size to e on every SIGWINCH until
// the command ends.
func kubeExecResizes(e *podwire.Exec) {
	ch := make(chan os.Signal, 1)
	signalNotify(ch, winchSignal)
	defer signal.Stop(ch)
	for {
		select {
		case <-e.Done():
			return
		case <-ch:
			if rows, cols := kubeExecSize(); rows > 0 && cols > 0 {
				_ = e.Resize(rows, cols)
			}
		}
	}
}

// kubeExecRawResizes is a channel of the terminal's size, pushed at the start
// and on every SIGWINCH, for the raw path's kube exec; closed when ctx ends.
func kubeExecRawResizes(ctx context.Context) <-chan kubeexec.TerminalSize {
	out := make(chan kubeexec.TerminalSize, 1)
	if rows, cols := kubeExecSize(); rows > 0 && cols > 0 {
		out <- kubeexec.TerminalSize{Rows: rows, Cols: cols}
	}
	ch := make(chan os.Signal, 1)
	signalNotify(ch, winchSignal)
	go func() {
		defer signal.Stop(ch)
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				rows, cols := kubeExecSize()
				if rows == 0 || cols == 0 {
					continue
				}
				select {
				case out <- kubeexec.TerminalSize{Rows: rows, Cols: cols}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

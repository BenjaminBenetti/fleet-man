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
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// kube_exec_direct.go is `fleet kube-exec`'s direct mode and `logs`: a
// terminal attach (and the raw fallback) straight to the API server with the
// transport fleetd itself uses, re-attached after a drop so a pane outlives
// an apiserver rollout, a network blip or a fleetd restart; and the saved +
// live container logs. The cluster is reached only through internal/kubeexec
// (the client-go leaf; the transport is opaque here), so the -tags nok8s pod
// build gets a stub (kube_exec_nok8s.go) — the pod never runs the shim.

// redialBackoff paces re-dials after a drop.
var redialBackoff = []time.Duration{0, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// runKubeExecDirect attaches argv to the pod over the API server. On a TTY the
// local terminal is put into raw mode and resizes are forwarded; raw mode (no
// fleet binary in the pod) runs the command with the pod's own shell and does
// not resume.
func runKubeExecDirect(ctx context.Context, f kubeExecFlags, argv []string, tty bool) (int, error) {
	t, podUID, err := kubeExecTransport(ctx, f, false)
	if err != nil {
		return -1, err
	}
	if tty {
		if restore := kubeExecRawTerminal(); restore != nil {
			defer restore()
		}
	}
	if f.raw {
		return runKubeExecRaw(ctx, t, f, argv, tty)
	}
	return runKubeExecAgent(ctx, t, f, argv, tty, podUID)
}

// runKubeExecAgent attaches through `pod-agent exec --stdio`, so the command
// is held and re-attached after a dropped connection.
func runKubeExecAgent(ctx context.Context, t *kubeexec.Transport, f kubeExecFlags, argv []string, tty bool, podUID string) (int, error) {
	rows, cols := kubeExecSize()
	header := podwire.ExecHeader{
		Request:   execas.Request{Class: execas.Class(f.class), User: f.user, Cwd: f.cwd, Argv: argv, TTY: tty, Rows: rows, Cols: cols},
		Resumable: true,
		PodUID:    podUID,
		ProtoMin:  podwire.ProtoMin,
		ProtoMax:  podwire.ProtoMax,
	}
	stream := t.Stream(ctx, podAgentStdioReq(f))
	e, err := podwire.OpenExec(ctx, stream, header, podwire.ExecOptions{Heartbeat: true})
	if err != nil {
		_ = stream.Close()
		if isAgentMissing(err) {
			// The pod has no fleet binary: run with its own shell instead.
			return runKubeExecRaw(ctx, t, f, argv, tty)
		}
		var se *podwire.StartError
		if errors.As(err, &se) {
			fmt.Fprintln(os.Stderr, se.Message)
			return se.Code, nil
		}
		return -1, err
	}
	go bridgeStdin(e)
	if tty {
		go kubeExecResizes(e)
	}
	go func() { _, _ = io.Copy(os.Stdout, e.Stdout()) }()
	go func() { _, _ = io.Copy(os.Stderr, e.Stderr()) }()
	go kubeExecResume(ctx, e, t, f)
	code, werr := e.Wait()
	_ = stream.Close()
	if errors.Is(werr, podwire.ErrHeldStreamGone) {
		// The pod was replaced; the shell ended with it. Exit 75 so the
		// pane's wrapper rejoins a fresh session.
		fmt.Fprintln(os.Stderr, "connection lost: the instance's pod was replaced")
		return podwire.ExitHeldStreamGone, nil
	}
	if errors.Is(werr, podwire.ErrConnectionLost) {
		fmt.Fprintln(os.Stderr, "connection lost")
		return 1, nil
	}
	return code, nil
}

// kubeExecResume re-attaches e to a fresh connection whenever it loses its
// transport, until it ends: a new credential (fleetd mints it from its cache,
// or the kubeconfig while fleetd is down) and a new API server exec.
func kubeExecResume(ctx context.Context, e *podwire.Exec, t *kubeexec.Transport, f kubeExecFlags) {
	for {
		select {
		case <-e.Done():
			return
		case <-e.Detached():
		}
		attempt := 0
		for {
			select {
			case <-e.Done():
				return
			case <-time.After(redialBackoff[min(attempt, len(redialBackoff)-1)]):
			}
			attempt++
			if fresh, _, err := kubeExecTransport(ctx, f, true); err == nil {
				t = fresh
			}
			stream := t.Stream(ctx, podAgentStdioReq(f))
			err := e.ReattachOver(ctx, stream)
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
	return -1, err
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
	t, _, err := kubeExecTransport(ctx, f.kubeExecFlags, false)
	if err != nil {
		// Without a cluster the saved log is all there is (a stopped
		// instance on an unreachable cluster).
		return nil
	}
	err = t.PodLogs(ctx, kubeexec.PodLogsRequest{Namespace: f.namespace, Pod: f.pod, Container: f.container, Follow: f.follow}, os.Stdout)
	if err != nil && !isPodNotRunning(err) {
		return err
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

// kubeExecTransport is the transport an attach reaches the pod with: fleetd's
// own (IssueKubeExecCredential, which verifies the pod and returns its uid),
// or, when fleetd will not answer, the connection's kubeconfig loaded here.
func kubeExecTransport(ctx context.Context, f kubeExecFlags, refresh bool) (*kubeexec.Transport, string, error) {
	if conn, err := fleetclient.DialLocal(ctx); err == nil {
		defer conn.Close()
		reply, err := conn.Service().IssueKubeExecCredential(ctx, &fleetgrpc.IssueKubeExecCredentialRequest{
			ClusterId: f.cluster, Pod: f.pod, Refresh: refresh,
		})
		if err == nil {
			t, cerr := kubeexec.TransportFor(protoconv.KubeExecCredentialFromProto(reply.GetCredential()))
			if cerr == nil {
				return t, reply.GetPodUid(), nil
			}
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

// podAgentStdioReq is the kube exec that runs the pod's fleet binary as the
// direct-exec agent.
func podAgentStdioReq(f kubeExecFlags) kubeexec.ExecRequest {
	return kubeexec.ExecRequest{Namespace: f.namespace, Pod: f.pod, Container: f.container, Command: []string{podseed.CurrentBinary, "pod-agent", "exec", "--stdio"}}
}

// isAgentMissing reports whether err is the agent exec never starting because
// the pod has no fleet binary.
func isAgentMissing(err error) bool {
	var se *podwire.StartError
	if errors.As(err, &se) {
		return se.Code == execas.ExitNotFound || se.Code == execas.ExitCannotRun
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

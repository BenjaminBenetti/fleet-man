//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// FallbackExec runs req in the pod while it has no agent session: one kube
// exec of `fleet exec-as`, the request framed on its stdin (never in argv,
// which the API server logs), followed by stdio.Stdin. Each one is an API
// server exec upgrade, so they are paced per connection. The pod is verified
// first. It returns the command's exit code (exec-as exits 125/126/127 as
// `docker exec` does when the command cannot start). A TTY request is
// refused: those go through an agent.
func (p *Pool) FallbackExec(ctx context.Context, t PodTarget, req execas.Request, stdio kubeexec.Stdio) (int, error) {
	if req.TTY {
		return -1, errors.New("a terminal needs the pod agent; the fallback runs commands without one")
	}
	var frame bytes.Buffer
	if err := execas.WriteRequest(&frame, req); err != nil {
		return -1, err
	}
	stdin := io.Reader(&frame)
	if stdio.Stdin != nil {
		stdin = io.MultiReader(&frame, stdio.Stdin)
	}
	stdio.Stdin = stdin
	return p.paced(ctx, t, []string{p.opts.Layout.Binary(), "exec-as"}, stdio)
}

// RawScript runs script with the pod's own sh while it has no usable fleet
// binary (it is missing, or not one this fleetd can talk to): the script
// travels on stdin, so argv names only the shell. Paced and verified like
// FallbackExec.
func (p *Pool) RawScript(ctx context.Context, t PodTarget, script string, stdout, stderr io.Writer) (int, error) {
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	return p.paced(ctx, t, []string{"sh", "-s"}, kubeexec.Stdio{Stdin: strings.NewReader(script), Stdout: stdout, Stderr: stderr})
}

// paced runs argv in the target's verified pod, one kube exec at a time per
// connection. Its errors carry their kubernetes: code: the fallback is how a
// command reports a cluster that went away, so its error is what the user
// sees (kubernetes:unreachable rather than client-go's dial error).
func (p *Pool) paced(ctx context.Context, t PodTarget, argv []string, stdio kubeexec.Stdio) (int, error) {
	code, err := p.pacedExec(ctx, t, argv, stdio)
	return code, classify(err)
}

func (p *Pool) pacedExec(ctx context.Context, t PodTarget, argv []string, stdio kubeexec.Stdio) (int, error) {
	c, err := p.Cluster(t.Cluster)
	if err != nil {
		return -1, err
	}
	pod, err := p.verify(ctx, c, t)
	if err != nil {
		return -1, err
	}
	if err := c.gates.fallback.wait(ctx); err != nil {
		return -1, err
	}
	if stdio.Stdout == nil {
		stdio.Stdout = io.Discard
	}
	if stdio.Stderr == nil {
		stdio.Stderr = io.Discard
	}
	req := kubeexec.ExecRequest{Namespace: pod.Namespace, Pod: pod.Name, Container: DevContainer, Command: argv, Stdin: stdio.Stdin != nil}
	err = p.exec(ctx, c, req, stdio)
	var exit *kubeexec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.Code, nil
	}
	return -1, err
}

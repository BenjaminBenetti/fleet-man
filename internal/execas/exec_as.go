package execas

import (
	"context"
	"errors"
	"os"
)

// ExecAsOptions configure RunExecAs.
type ExecAsOptions struct {
	// SpecPath is the exec spec to read (DefaultSpecPath in a pod).
	SpecPath string
	// CacheDir is the probe cache directory ("" keeps no cache on disk).
	CacheDir string
}

// RunExecAs is `fleet exec-as`, the transport-free way into a pod for when the
// agent session is down: fleetd kube-execs it, writes one request frame on its
// stdin, then the command's own input. It reads the frame, plans the request
// against the pod and execs the command in place of itself, so the command
// inherits the rest of stdin, stdout and stderr, and its exit status is the
// kube exec's. It returns only on failure, with an *ExitError whose code is
// docker's for the same failure. A terminal on stdin is refused, as its line
// discipline would mangle the frame; so is a TTY request, as a PTY needs a
// process to serve it, which a command exec'd in place cannot have (TTY
// attaches go through the pod agent).
func RunExecAs(ctx context.Context, stdin *os.File, opts ExecAsOptions) error {
	if isTerminal(stdin) {
		return failed(errors.New("exec-as reads a framed request on stdin, which must not be a terminal"))
	}
	req, err := ReadRequest(stdin)
	if err != nil {
		return failed(err)
	}
	if req.TTY {
		return failed(errors.New("exec-as runs no tty commands; they go through the pod agent"))
	}
	spec, err := LoadSpec(opts.SpecPath)
	if err != nil {
		return failed(err)
	}
	plan, err := NewHost(spec, opts.CacheDir).Plan(ctx, req)
	if err != nil {
		return err
	}
	return Exec(plan)
}

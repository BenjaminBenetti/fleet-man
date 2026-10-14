package kubeshim

import (
	"context"
	"io"
)

// Execer runs the `docker exec` calls the pod-mode shim receives, in the
// instance's pod. They are `docker exec` semantics, the agent's explicit exec
// class: the dev container's environment plus Env, as User (the container's
// user when empty), in Dir (its working directory when empty). The cluster
// backend binds one to the pod's agent session and hands it to devcli.SetUp,
// which serves it on the shim directory's exec socket (ServeExec).
type Execer interface {
	// Exec runs req and returns the command's exit code once it has ended
	// and all of its output is written to stdio, without waiting for stdin
	// to end (the CLI keeps a shell server's stdin open). err is reserved for
	// failing to run it at all (the pod is gone, the session dropped); ctx is
	// cancelled when the shim that asked goes away, and the command must then
	// be ended.
	Exec(ctx context.Context, req ExecRequest, stdio ExecStdio) (exitCode int, err error)
}

// ExecerFunc adapts a function to Execer.
type ExecerFunc func(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error)

// Exec calls f.
func (f ExecerFunc) Exec(ctx context.Context, req ExecRequest, stdio ExecStdio) (int, error) {
	return f(ctx, req, stdio)
}

// ExecRequest is one `docker exec -i -t -u -e -w <container> <argv...>`.
type ExecRequest struct {
	// User is -u ("name", "uid", "name:group"); "" means the container's.
	User string `json:"user,omitempty"`
	// Env is -e and --env-file, as NAME=VALUE, applied over the container's
	// environment.
	Env []string `json:"env,omitempty"`
	// Dir is -w; "" means the container's working directory.
	Dir string `json:"dir,omitempty"`
	// Argv is the command.
	Argv []string `json:"argv"`
	// Stdin is -i: the command reads the caller's stdin. Without it the
	// command's stdin is empty.
	Stdin bool `json:"stdin,omitempty"`
	// TTY is -t: the command runs on a pseudo-terminal, whose output comes
	// back as stdout alone.
	TTY bool `json:"tty,omitempty"`
	// Size is the terminal's starting size when TTY is set and the caller's
	// stdin is a terminal.
	Size *WindowSize `json:"size,omitempty"`
}

// WindowSize is a terminal size.
type WindowSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// ExecStdio is an exec's streams.
type ExecStdio struct {
	// Stdin is the caller's stdin, which ends (io.EOF) when the caller closes
	// it; nil unless ExecRequest.Stdin.
	Stdin io.Reader
	// Stdout and Stderr take the command's output. Writes fail once the
	// caller is gone.
	Stdout io.Writer
	Stderr io.Writer
	// Resize delivers the caller's terminal resizes (TTY only); it is never
	// closed.
	Resize <-chan WindowSize
}

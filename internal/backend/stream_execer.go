package backend

import (
	"context"
	"io"
)

// StreamExecer is an optional capability for backends that run a command
// inside an instance as a stream the daemon holds itself, instead of as a host
// process (a `docker exec`) whose stdio the daemon pipes. Kubernetes pods run
// it through their agent session: the command, its PTY and its process group
// live in the pod, and no host-side helper runs per command.
//
// The daemon's long-lived streams prefer it when a backend has it: the Exec RPC
// (remote shells, `fleet shell`), and the virtual microphone's sink and the
// audio output's source. Backends without it keep running ExecCommandQuiet,
// MicSinkCommand and OutputSourceCommand on the daemon host.
type StreamExecer interface {
	// OpenExec starts req's command and returns its stream. ctx bounds the
	// start only: once OpenExec has returned, the command runs until it exits
	// or the stream is closed, whatever becomes of ctx.
	OpenExec(ctx context.Context, req ExecRequest) (ExecStream, error)
}

// ExecClass says whom a StreamExecer runs a command as, and with which
// environment: the two ways fleet already reaches into an instance.
type ExecClass uint8

const (
	// ExecClassExec runs the command the way ExecCommand does, as
	// `devcontainer exec` would: as the remoteUser, with the user's probed
	// environment and the config's remoteEnv, in the workspace folder.
	ExecClassExec ExecClass = iota
	// ExecClassScript runs it the way RunScript and MicSinkCommand do: as the
	// session user, straight against the container (`docker exec -u`), with
	// no devcontainer environment.
	ExecClassScript
)

// ExecRequest is one command for a StreamExecer.
type ExecRequest struct {
	// Class is whom the command runs as, and how.
	Class ExecClass
	// WorkspaceDir and ContainerID name the instance the way ExecCommand
	// (by workspace folder) and RunScript (by container) do. Callers set
	// both; a backend uses what it needs.
	WorkspaceDir string
	ContainerID  string
	// Argv is the command line; it is not run through a shell.
	Argv []string
	// Env is added to the command's environment last, so it wins.
	Env map[string]string
	// TTY runs the command on a pseudo-terminal: its output then comes merged
	// on Stdout, and Resize sets the window size.
	TTY bool
}

// ExecStream is a command a StreamExecer started. Its methods may be called
// from different goroutines (one feeding Stdin, others draining Stdout and
// Stderr, another closing it), but each of Stdin, Stdout and Stderr from one
// goroutine at a time.
type ExecStream interface {
	// Stdin feeds the command. Closing it half-closes: the command reads EOF
	// and keeps running. With a TTY there is no half-close and closing it does
	// nothing; input ends in-band (^D).
	Stdin() io.WriteCloser
	// Stdout is the command's output, read until it returns an error (io.EOF
	// once the command is done). With a TTY it carries stdout and stderr
	// merged, as a terminal shows them.
	Stdout() io.Reader
	// Stderr is the command's error output. With a TTY it is empty.
	Stderr() io.Reader
	// Resize sets the TTY's window size. Without a TTY it does nothing.
	Resize(rows, cols uint16) error
	// Wait waits for the command to exit, releases the stream, and returns
	// the exit code: -1 when a signal killed it. err is non-nil, with code -1,
	// only when no exit status could be had (the stream broke). Read Stdout
	// and Stderr to the end first: output still unread may be lost. Call it
	// exactly once, after Close too.
	Wait() (code int, err error)
	// Close aborts the command if it is still running (its whole process
	// group, where the backend can reach it). It may be called more than once
	// and concurrently with the other methods, and does nothing once Wait has
	// returned; Wait still has to be called after it.
	Close() error
}

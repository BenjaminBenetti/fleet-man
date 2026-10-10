package execas

import (
	"errors"
	"syscall"
)

// ExitStatus is how a started command ended.
type ExitStatus struct {
	// Code is the exit code; -1 when a signal killed the command.
	Code int
	// Signal is the signal that killed it; 0 when it exited.
	Signal syscall.Signal
}

// ShellCode is the status the way a shell (and `docker exec`) reports it: the
// exit code, or 128 plus the signal that killed the command.
func (s ExitStatus) ShellCode() int {
	if s.Signal != 0 {
		return 128 + int(s.Signal)
	}
	return s.Code
}

// Exit codes for a command execas could not start, the ones `docker exec`
// gives for the same failures.
const (
	// ExitFailed: the request, the spec or execas itself failed (docker's
	// own errors).
	ExitFailed = 125
	// ExitCannotRun: the command could not be invoked: an unknown user, a
	// working directory that is missing or not allowed, an executable the user
	// may not run (docker's "OCI runtime exec failed").
	ExitCannotRun = 126
	// ExitNotFound: the executable does not exist.
	ExitNotFound = 127
)

// ExitError is a failure to start a command, with the exit code `docker exec`
// would have returned for it.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode is the exit code a failure maps to: an ExitError's own code,
// ExitFailed for any other error, 0 for nil.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return ExitFailed
}

func failed(err error) error    { return &ExitError{Code: ExitFailed, Err: err} }
func cannotRun(err error) error { return &ExitError{Code: ExitCannotRun, Err: err} }
func notFound(err error) error  { return &ExitError{Code: ExitNotFound, Err: err} }

package kubeerr

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// IsUnreachable reports whether err is a failure to reach the API server at
// all: the connection was refused or reset, no route leads to the host or
// its network, the name did not resolve, the dial or a read timed out (any
// net.Error, which client-go's *url.Error is). A context's end is not one
// (the caller gave up, not the cluster), except a dial that timed out: no
// connection was made either way, and net reports a dialer's own timeout as
// context.DeadlineExceeded about half the time (it races the deadline it
// sets). A cancelled dial is the caller's.
func IsUnreachable(err error) bool {
	var netErr net.Error
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return false
	case dialFailed(err):
		return true
	case errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return errors.As(err, &netErr) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}

// dialFailed reports whether err is a connection that was never made.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// Unreached is err coded Unreachable when the API server was not reached
// (IsUnreachable) and err has no code yet; any other error comes back as is.
// A caller that reaches a cluster itself (the kube-exec shim's direct mode)
// codes its failures with it; the daemon's own calls classify their errors
// more finely.
func Unreached(err error) error {
	if !IsUnreachable(err) {
		return err
	}
	return Wrap(Unreachable, err)
}

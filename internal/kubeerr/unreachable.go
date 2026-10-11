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
// net.Error, which client-go's *url.Error is). A context's end is not one:
// the caller gave up, not the cluster.
func IsUnreachable(err error) bool {
	var netErr net.Error
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return errors.As(err, &netErr) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
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

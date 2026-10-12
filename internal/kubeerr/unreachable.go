package kubeerr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
)

// ErrNoAnswer is an API server that took the connection but did not answer
// in time (its TLS handshake, or the upgrade to an exec stream, hung): as
// unreachable as one that never took it.
var ErrNoAnswer = errors.New("the API server did not answer")

// IsUnreachable reports whether err is a failure to reach the API server at
// all: the connection was refused or reset, no route leads to the host or
// its network, the name did not resolve, the dial or a read timed out (any
// net.Error, which client-go's *url.Error is). A context's end is not one
// (the caller gave up, not the cluster), except a dial that timed out: no
// connection was made either way, and net reports a dialer's own timeout as
// context.DeadlineExceeded about half the time (it races the deadline it
// sets). A cancelled dial is the caller's; one that connected but got no
// answer in time (ErrNoAnswer) is the cluster's. Nor is a server that was reached
// but not trusted, or that refused the client's certificate
// (IsCertificateRefused), or a credential plugin that failed before the
// request went out (IsCredentialFailure): a *url.Error carries those too.
func IsUnreachable(err error) bool {
	var netErr net.Error
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return false
	case dialFailed(err), noAnswer(err):
		return true
	case errors.Is(err, context.DeadlineExceeded), IsCertificateRefused(err), IsCredentialFailure(err):
		return false
	}
	return errors.As(err, &netErr) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}

// noAnswer reports whether err is ErrNoAnswer, also once a client flattened
// it into its own message (client-go's SPDY dialer does).
func noAnswer(err error) bool {
	return errors.Is(err, ErrNoAnswer) || strings.Contains(err.Error(), ErrNoAnswer.Error())
}

// dialFailed reports whether err is a connection that was never made.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// IsCertificateRefused reports whether err is the TLS handshake with the API
// server failing on a certificate: the server's did not verify against the
// connection's certificate authority (or names another host), or the server
// refused the client's (an alert: a certificate it does not take, or one
// that expired).
func IsCertificateRefused(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		invalid   x509.CertificateInvalidError
		host      x509.HostnameError
	)
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &invalid) ||
		errors.As(err, &host) || serverRefusedClient(err)
}

// serverRefusedClient reports whether err is the API server refusing the
// client's certificate (a certificate alert: bad, unsupported, revoked,
// expired, unknown, of an unknown authority, or missing), as opposed to the
// client not trusting the server's.
func serverRefusedClient(err error) bool {
	// crypto/tls hands a TCP connection's alert back as a "remote error"
	// around a type of its own, whose text names it ("tls: expired
	// certificate"); the certificate alerts all say "certificate".
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error" && op.Err != nil &&
		strings.Contains(op.Err.Error(), "certificate")
}

// IsCredentialFailure reports whether err is client-go's credential plugin
// failing ("getting credentials: exec: ..."): a login to renew, not a
// cluster that is away.
func IsCredentialFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "getting credentials")
}

// Transport is the class of err as a failure to ask the API server at all
// ("" when err is none): Unauthorized when the credential plugin failed, or
// the server refused the client's certificate (a login to renew);
// Misconfigured when the server's certificate did not verify (the
// connection's certificate authority or server is wrong); Unreachable when
// the server was not reached (IsUnreachable).
func Transport(err error) Code {
	switch {
	case err == nil:
		return ""
	case IsCredentialFailure(err), serverRefusedClient(err):
		return Unauthorized
	case IsCertificateRefused(err):
		return Misconfigured
	case IsUnreachable(err):
		return Unreachable
	}
	return ""
}

// Unreached is err coded with its Transport class when it has one and no
// code yet; any other error comes back as is. A caller that reaches a
// cluster itself (the kube-exec shim's direct mode) codes its failures with
// it; the daemon's own calls classify their errors more finely.
func Unreached(err error) error {
	if c := Transport(err); c != "" {
		return Wrap(c, err)
	}
	return err
}

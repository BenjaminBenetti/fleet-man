package fleetclient

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc/status"
)

// ssh_hostkey.go is the client side of the ssh host-key prompt. The local
// daemon refuses an unknown host key (strict checking, no accept-new) and
// reports what the host presented as an UnknownSSHHostKey status detail;
// UnknownSSHHostKey extracts it from a Dial/Ping error so an interactive
// client can show the fingerprint and ask, and TrustSSHHostKey is the accept.
// A non-interactive client (the CLI with FLEET_SSH set) just fails with the
// status message, which already says how to trust the key.

// UnknownSSHHostKey returns the detail attached to err when the local daemon
// refused an ssh:// remote for an unknown host key, or nil for any other
// error. Works through the wrapping Dial/Ping apply.
func UnknownSSHHostKey(err error) *fleetgrpc.UnknownSSHHostKey {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, d := range st.Details() {
		if uk, ok := d.(*fleetgrpc.UnknownSSHHostKey); ok {
			return uk
		}
	}
	return nil
}

// TrustSSHHostKey asks the LOCAL daemon to append one offered known_hosts line
// for url and resolve the remote again. Returns nil once the remote answered
// through the freshly trusted tunnel. Package var so TUI tests can stub it.
var TrustSSHHostKey = func(ctx context.Context, url, knownHostsLine string) error {
	rctx, cancel := context.WithTimeout(ctx, SSHResolveTimeout)
	defer cancel()
	conn, err := DialLocal(rctx)
	if err != nil {
		return fmt.Errorf("local fleet daemon (owns the ssh tunnel): %w", err)
	}
	defer conn.Close()
	_, err = conn.Service().TrustSSHHostKey(rctx, &fleetgrpc.TrustSSHHostKeyRequest{Url: url, KnownHostsLine: knownHostsLine})
	return err
}

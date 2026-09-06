package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// armada.go implements the fleet-armada registry RPCs (armada.json). The
// registry is client-side data — the list of remote fleets THIS machine's user
// can switch to — so the TUI always sends these to the LOCAL daemon, even
// while its main connection points at a remote fleet. The handlers themselves
// can't tell (and don't care) which transport they arrived on; locality is the
// client's responsibility (fleetclient.DialLocal).

// GetArmada returns the registered remote fleets (empty when armada.json is
// absent).
func (s *service) GetArmada(_ context.Context, _ *fleetgrpc.GetArmadaRequest) (*fleetgrpc.GetArmadaReply, error) {
	a, err := state.LoadArmada()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load armada: %v", err)
	}
	return &fleetgrpc.GetArmadaReply{Remotes: armadaToProto(a)}, nil
}

// SetArmada replaces the whole registry (the settings page sends the full
// edited list). muWrite serializes it alongside config.json writes — both are
// small whole-file replaces owned by this server.
func (s *service) SetArmada(_ context.Context, req *fleetgrpc.SetArmadaRequest) (*fleetgrpc.SetArmadaReply, error) {
	s.muWrite.Lock()
	defer s.muWrite.Unlock()

	if err := state.SaveArmada(protoToArmada(req.GetRemotes())); err != nil {
		return nil, status.Errorf(codes.Internal, "save armada: %v", err)
	}
	saved, err := state.LoadArmada()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload armada: %v", err)
	}

	// Never log URLs-with-tokens or tokens; the count is the useful signal.
	flog.Info("armada updated", "remotes", len(saved.Remotes))

	// Drop the ssh forwards of remotes that were just removed (a forward to a
	// remote the user deleted has no owner left). nil in newService() tests.
	if s.sshTunnels != nil {
		keep := make([]string, 0, len(saved.Remotes))
		for _, r := range saved.Remotes {
			keep = append(keep, r.URL)
		}
		s.sshTunnels.Prune(keep)
	}

	return &fleetgrpc.SetArmadaReply{Remotes: armadaToProto(saved)}, nil
}

// ResolveArmadaRemote brings up (or reuses) the local ssh forward for an ssh://
// remote and returns the loopback address + bearer token a client dials (see
// internal/server/sshtunnel). LOCAL-ONLY (remote_auth.go): it returns a
// credential and runs the user's ssh. Failures are FailedPrecondition with a
// user-facing message — the settings page shows it verbatim as the remote's
// status — except a malformed URL, which is InvalidArgument.
func (s *service) ResolveArmadaRemote(ctx context.Context, req *fleetgrpc.ResolveArmadaRemoteRequest) (*fleetgrpc.ResolveArmadaRemoteReply, error) {
	if !sshtunnel.IsSSHURL(req.GetUrl()) {
		return nil, status.Errorf(codes.InvalidArgument, "not an ssh:// remote: %q", req.GetUrl())
	}
	if _, err := sshtunnel.ParseURL(req.GetUrl()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.sshTunnels == nil {
		return nil, status.Error(codes.Unavailable, "ssh tunnels are not available on this daemon")
	}
	ep, err := s.sshTunnels.Resolve(ctx, req.GetUrl())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &fleetgrpc.ResolveArmadaRemoteReply{Addr: ep.Addr, Token: ep.Token}, nil
}

func armadaToProto(a *state.Armada) []*fleetgrpc.ArmadaRemote {
	if a == nil {
		return nil
	}
	out := make([]*fleetgrpc.ArmadaRemote, 0, len(a.Remotes))
	for _, r := range a.Remotes {
		out = append(out, &fleetgrpc.ArmadaRemote{Url: r.URL, Token: r.Token})
	}
	return out
}

func protoToArmada(remotes []*fleetgrpc.ArmadaRemote) *state.Armada {
	a := &state.Armada{}
	for _, r := range remotes {
		a.Remotes = append(a.Remotes, state.ArmadaRemote{URL: r.GetUrl(), Token: r.GetToken()})
	}
	return a
}
